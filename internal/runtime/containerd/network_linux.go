package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"

	gocni "github.com/containerd/go-cni"

	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// network attaches containers to a node-local bridge via CNI plugins:
// bridge + host-local IPAM, portmap for host ports, firewall so traffic is
// accepted even when the FORWARD policy is DROP (e.g. with Docker installed).
type network struct {
	cni  gocni.CNI
	name string // CNI network name, part of the plugins' iptables comments
}

func newNetwork(o Options) (*network, error) {
	prefix, err := netip.ParsePrefix(o.PodCIDR)
	if err != nil || !prefix.Addr().Is4() {
		return nil, fmt.Errorf("invalid pod CIDR %q: want an IPv4 prefix", o.PodCIDR)
	}
	if err := checkBridge(o.BridgeName, prefix.Masked()); err != nil {
		return nil, err
	}
	conf := map[string]any{
		"cniVersion": "1.0.0",
		"name":       o.NetworkName,
		"plugins": []map[string]any{
			{
				"type":        "bridge",
				"bridge":      o.BridgeName,
				"isGateway":   true,
				"ipMasq":      true,
				"hairpinMode": true,
				"ipam": map[string]any{
					"type":    "host-local",
					"ranges":  [][]map[string]string{{{"subnet": prefix.Masked().String()}}},
					"routes":  []map[string]string{{"dst": "0.0.0.0/0"}},
					"dataDir": filepath.Join(o.StateDir, "cni", "ipam"),
				},
			},
			{"type": "portmap", "capabilities": map[string]bool{"portMappings": true}},
			{"type": "firewall"},
		},
	}
	b, err := json.Marshal(conf)
	if err != nil {
		return nil, fmt.Errorf("encode CNI config: %w", err)
	}
	c, err := gocni.New(
		gocni.WithPluginDir([]string{o.CNIBinDir}),
		gocni.WithMinNetworkCount(2),
		gocni.WithLoNetwork,
		gocni.WithConfListBytes(b),
	)
	if err != nil {
		return nil, fmt.Errorf("init CNI: %w", err)
	}
	return &network{cni: c, name: o.NetworkName}, nil
}

// setup attaches the network namespace at netnsPath and returns the
// container's IPv4 address.
func (n *network) setup(ctx context.Context, id, netnsPath string, ports []runtime.PortMapping) (string, error) {
	res, err := n.cni.Setup(ctx, id, netnsPath, gocni.WithCapabilityPortMap(cniPorts(ports)))
	if err != nil {
		return "", fmt.Errorf("CNI setup: %w", err)
	}
	for name, iface := range res.Interfaces {
		if name == "lo" {
			continue
		}
		for _, ipc := range iface.IPConfigs {
			if ip4 := ipc.IP.To4(); ip4 != nil {
				return ip4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("CNI setup: no IPv4 address assigned")
}

// teardown releases the container's network: IP, port mappings and, given
// the pinned netns path, the veth and the masquerade rules. With an empty
// path (the netns is already gone, e.g. after a reboot) plugins can only
// release IPAM and port mappings; the bridge plugin then skips its iptables
// cleanup, which is why containers get a pinned netns. Idempotent.
func (n *network) teardown(ctx context.Context, id, netnsPath string, ports []runtime.PortMapping) error {
	if err := n.cni.Remove(ctx, id, netnsPath, gocni.WithCapabilityPortMap(cniPorts(ports))); err != nil {
		return fmt.Errorf("CNI teardown: %w", err)
	}
	return nil
}

// purgeMasquerade removes the bridge plugin's masquerade rules and chain for
// container id. The plugin removes them itself only when it can enter the
// container's netns; this covers a netns that was lost while iptables kept
// its rules (e.g. the state dir was wiped), and rules leaked by versions
// before the pinned netns. Leftovers would otherwise block deleting the
// chain, and with it every later teardown of the same container ID. After a
// reboot there is nothing to remove.
func (n *network) purgeMasquerade(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "iptables", "-w", "-t", "nat", "-S", "POSTROUTING").Output()
	if err != nil {
		return fmt.Errorf("list NAT rules: %w", err)
	}
	comment := fmt.Sprintf(`name: "%s" id: "%s"`, n.name, id)
	chains := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		args := splitIptablesRule(line)
		if len(args) < 2 || args[0] != "-A" || !ruleHasComment(args, comment) {
			continue
		}
		// Delete by full spec, not by number: other software may change the
		// table meanwhile.
		args[0] = "-D"
		if err := exec.CommandContext(ctx, "iptables", append([]string{"-w", "-t", "nat"}, args...)...).Run(); err != nil {
			return fmt.Errorf("delete stale NAT rule for %s: %w", id, err)
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-j" && strings.HasPrefix(args[i+1], "CNI-") {
				chains[args[i+1]] = true
			}
		}
	}
	for chain := range chains {
		// Ignore errors: the chain may already be gone, and a chain that is
		// still in use is not ours to remove.
		_ = exec.CommandContext(ctx, "iptables", "-w", "-t", "nat", "-F", chain).Run()
		_ = exec.CommandContext(ctx, "iptables", "-w", "-t", "nat", "-X", chain).Run()
	}
	return nil
}

func ruleHasComment(args []string, comment string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--comment" && args[i+1] == comment {
			return true
		}
	}
	return false
}

// splitIptablesRule splits a line of `iptables -S` output into arguments,
// honouring its double quotes with backslash escapes.
func splitIptablesRule(line string) []string {
	var (
		args     []string
		cur      strings.Builder
		inQuotes bool
		hasArg   bool
	)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case inQuotes && ch == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case ch == '"':
			inQuotes, hasArg = !inQuotes, true
		case ch == ' ' && !inQuotes:
			if hasArg {
				args = append(args, cur.String())
				cur.Reset()
				hasArg = false
			}
		default:
			cur.WriteByte(ch)
			hasArg = true
		}
	}
	if hasArg {
		args = append(args, cur.String())
	}
	return args
}

// checkBridge fails early if the bridge exists with an address outside
// prefix (e.g. after changing --pod-cidr); otherwise every container start
// would fail in the bridge plugin.
func checkBridge(name string, prefix netip.Prefix) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil // not created yet; the bridge plugin will create it
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("read addresses of bridge %s: %w", name, err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil {
			continue
		}
		ip, _ := netip.AddrFromSlice(ipn.IP.To4())
		if !prefix.Contains(ip) {
			return fmt.Errorf("bridge %s has address %s outside pod CIDR %s; "+
				"use the previous --pod-cidr, or remove all containers and run `ip link delete %s`",
				name, ipn, prefix, name)
		}
	}
	return nil
}

func cniPorts(ports []runtime.PortMapping) []gocni.PortMapping {
	var out []gocni.PortMapping
	for _, p := range ports {
		if p.HostPort == 0 {
			continue
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, gocni.PortMapping{HostPort: p.HostPort, ContainerPort: p.ContainerPort, Protocol: proto})
	}
	return out
}
