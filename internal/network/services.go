// Package network sets up a node's cluster networking: Service load
// balancing (iptables), DNS for Service names, and the WireGuard mesh that
// connects the nodes' container subnets.
package network

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Service is a Service as the node load-balances it.
type Service struct {
	Name      string
	ClusterIP string
	Ports     []ServicePort
	Endpoints []string // IPs of ready replicas
}

// ServicePort maps a Service port to the replicas' target port.
type ServicePort struct {
	Port, TargetPort int
	Protocol         string // tcp or udp
}

// Chains owned by shoal. Everything in them is rewritten on each change.
const (
	chainServices    = "SHOAL-SERVICES"    // nat: Service IP:port -> per-port chain
	chainPostrouting = "SHOAL-POSTROUTING" // nat: masquerade service traffic, no NAT between pods
	chainForward     = "SHOAL-FORWARD"     // filter: reject Services without endpoints, allow cluster traffic (Docker sets FORWARD to DROP)
	chainOutput      = "SHOAL-OUTPUT"      // filter: reject Services without endpoints, from the node itself
	chainPrefix      = "SHOAL-SVC-"
	// markMasq marks connections to Services so their source is
	// masqueraded: replies then come back through the node that DNATed
	// them, also when client and replica share a bridge.
	markMasq = "0x4000/0x4000"
)

// ServiceProxy programs iptables so that a Service's IP and ports reach a
// random ready replica, from containers and from the node itself.
type ServiceProxy struct {
	clusterCIDR string
	mu          sync.Mutex
	applied     string // last rules applied, to skip no-op updates
	chains      []string
	// iptables runs an iptables command and returns its output;
	// replaceable in tests.
	iptables func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

// NewServiceProxy returns a proxy for a cluster whose container subnets
// are all within clusterCIDR.
func NewServiceProxy(clusterCIDR string) *ServiceProxy {
	return &ServiceProxy{clusterCIDR: clusterCIDR, iptables: runIptables}
}

func runIptables(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	name := "iptables"
	if len(args) > 0 && args[0] == "restore" {
		name, args = "iptables-restore", args[1:]
	}
	cmd := exec.CommandContext(ctx, name, append([]string{"-w"}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// existingChains lists our per-port chains already in the kernel (e.g.
// from before an agent restart).
func (p *ServiceProxy) existingChains(ctx context.Context) []string {
	out, err := p.iptables(ctx, nil, "-t", "nat", "-S")
	if err != nil {
		return nil
	}
	var chains []string
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "-N "+chainPrefix); ok {
			chains = append(chains, chainPrefix+strings.TrimSpace(name))
		}
	}
	return chains
}

func portChain(svc Service, p ServicePort) string {
	sum := sha256.Sum256([]byte(svc.Name + "/" + strconv.Itoa(p.Port) + "/" + p.Protocol))
	return chainPrefix + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

// render returns the iptables-restore input for services, and the
// per-port chains it defines.
func (p *ServiceProxy) render(services []Service) (string, []string) {
	services = slices.Clone(services)
	slices.SortFunc(services, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	var chains []string
	b.WriteString("*nat\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n:%s - [0:0]\n", chainServices, chainPostrouting)
	var rules strings.Builder
	for _, svc := range services {
		for _, sp := range svc.Ports {
			chain := portChain(svc, sp)
			chains = append(chains, chain)
			fmt.Fprintf(&b, ":%s - [0:0]\n", chain)
			fmt.Fprintf(&rules, "-A %s -d %s/32 -p %s --dport %d -m comment --comment \"%s:%d\" -j %s\n",
				chainServices, svc.ClusterIP, sp.Protocol, sp.Port, svc.Name, sp.Port, chain)
			fmt.Fprintf(&rules, "-A %s -j MARK --set-xmark %s\n", chain, markMasq)
			eps := slices.Sorted(slices.Values(svc.Endpoints))
			for i, ep := range eps {
				// Each rule takes 1/(remaining) of what's left: uniform overall.
				prob := ""
				if left := len(eps) - i; left > 1 {
					prob = fmt.Sprintf("-m statistic --mode random --probability %.5f ", 1/float64(left))
				}
				fmt.Fprintf(&rules, "-A %s -p %s %s-j DNAT --to-destination %s:%d\n", chain, sp.Protocol, prob, ep, sp.TargetPort)
			}
		}
	}
	b.WriteString(rules.String())
	fmt.Fprintf(&b, "-A %s -m mark --mark %s -j MASQUERADE\n", chainPostrouting, markMasq)
	fmt.Fprintf(&b, "-A %s -s %s -d %s -j ACCEPT\n", chainPostrouting, p.clusterCIDR, p.clusterCIDR)
	b.WriteString("COMMIT\n*filter\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n:%s - [0:0]\n", chainForward, chainOutput)
	// A Service without ready replicas refuses connections at once, so
	// clients fail fast and retry, instead of their packets disappearing
	// towards the default route.
	for _, svc := range services {
		if len(svc.Endpoints) > 0 {
			continue
		}
		for _, sp := range svc.Ports {
			reject := "icmp-port-unreachable"
			if sp.Protocol == "tcp" {
				reject = "tcp-reset"
			}
			for _, chain := range []string{chainForward, chainOutput} {
				fmt.Fprintf(&b, "-A %s -d %s/32 -p %s --dport %d -m comment --comment \"%s:%d has no endpoints\" -j REJECT --reject-with %s\n",
					chain, svc.ClusterIP, sp.Protocol, sp.Port, svc.Name, sp.Port, reject)
			}
		}
	}
	fmt.Fprintf(&b, "-A %s -s %s -j ACCEPT\n-A %s -d %s -j ACCEPT\n", chainForward, p.clusterCIDR, chainForward, p.clusterCIDR)
	b.WriteString("COMMIT\n")
	return b.String(), chains
}

// Sync makes iptables match services. Our chains are replaced atomically
// with iptables-restore; the jumps into them are added once.
func (p *ServiceProxy) Sync(ctx context.Context, services []Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rules, chains := p.render(services)
	if rules == p.applied {
		return nil
	}
	if p.applied == "" {
		p.chains = p.existingChains(ctx) // clean up after a previous agent process too
	}
	if _, err := p.iptables(ctx, []byte(rules), "restore", "--noflush"); err != nil {
		return err
	}
	for _, j := range []struct{ table, chain, target string }{
		{"nat", "PREROUTING", chainServices},
		{"nat", "OUTPUT", chainServices},
		{"nat", "POSTROUTING", chainPostrouting},
		{"filter", "FORWARD", chainForward},
		{"filter", "OUTPUT", chainOutput},
	} {
		if _, err := p.iptables(ctx, nil, "-t", j.table, "-C", j.chain, "-j", j.target); err != nil {
			if _, err := p.iptables(ctx, nil, "-t", j.table, "-I", j.chain, "1", "-j", j.target); err != nil {
				return err
			}
		}
	}
	// Chains of removed Services or ports are no longer referenced.
	for _, old := range p.chains {
		if !slices.Contains(chains, old) {
			// -X only deletes empty chains. Best effort: an unknown chain is fine.
			_, _ = p.iptables(ctx, nil, "-t", "nat", "-F", old)
			_, _ = p.iptables(ctx, nil, "-t", "nat", "-X", old)
		}
	}
	p.applied, p.chains = rules, chains
	return nil
}
