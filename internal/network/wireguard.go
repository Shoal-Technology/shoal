package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/Shoal-Technology/shoal/internal/security"
)

// Peer is another node of the mesh.
type Peer struct {
	Node      string
	PublicKey string
	Endpoint  string // host:port
	PodCIDR   string
}

// Mesh is the node's WireGuard interface. Every node peers with every other
// node and routes their container subnets through the tunnel, so
// containers reach each other across nodes with their own IPs.
type Mesh struct {
	iface   string
	port    int
	key     wgtypes.Key
	address string // the node's gateway IP, also used on the tunnel
	log     *slog.Logger
}

const keepalive = 25 * time.Second // keeps NAT mappings open

// NewMesh loads the node's private key from keyPath (creating it on first
// use; it never leaves the node).
func NewMesh(keyPath, iface string, port int, podCIDR string, log *slog.Logger) (*Mesh, error) {
	key, err := loadOrCreateWireguardKey(keyPath)
	if err != nil {
		return nil, err
	}
	gw, err := GatewayIP(podCIDR)
	if err != nil {
		return nil, err
	}
	return &Mesh{iface: iface, port: port, key: key, address: gw, log: log}, nil
}

// GatewayIP is the first address of a container subnet: the bridge's
// address and the node's address in the mesh.
func GatewayIP(podCIDR string) (string, error) {
	p, err := netip.ParsePrefix(podCIDR)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("invalid pod CIDR %q", podCIDR)
	}
	return p.Masked().Addr().Next().String(), nil
}

func loadOrCreateWireguardKey(path string) (wgtypes.Key, error) {
	if b, err := os.ReadFile(path); err == nil {
		k, err := wgtypes.ParseKey(strings.TrimSpace(string(b)))
		if err != nil {
			return wgtypes.Key{}, fmt.Errorf("WireGuard key %s: %w", path, err)
		}
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return wgtypes.Key{}, fmt.Errorf("read WireGuard key: %w", err)
	}
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generate WireGuard key: %w", err)
	}
	if err := security.WriteFileAtomic(path, []byte(k.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, err
	}
	return k, nil
}

// PublicKey is what the node announces to the other nodes.
func (m *Mesh) PublicKey() string { return m.key.PublicKey().String() }

// Port is the UDP port the node listens on.
func (m *Mesh) Port() int { return m.port }

func ip(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Sync creates the interface if needed and makes its peers and routes
// match peers.
func (m *Mesh) Sync(ctx context.Context, peers []Peer) error {
	if _, err := ip(ctx, "link", "show", "dev", m.iface); err != nil {
		if _, err := ip(ctx, "link", "add", "dev", m.iface, "type", "wireguard"); err != nil {
			return fmt.Errorf("create WireGuard interface (is the wireguard kernel module available?): %w", err)
		}
		m.log.Info("created WireGuard interface", "interface", m.iface, "port", m.port)
	}
	if _, err := ip(ctx, "address", "replace", m.address+"/32", "dev", m.iface); err != nil {
		return err
	}
	if _, err := ip(ctx, "link", "set", "dev", m.iface, "up"); err != nil {
		return err
	}

	cfg := wgtypes.Config{PrivateKey: &m.key, ListenPort: &m.port, ReplacePeers: true}
	ka := keepalive
	want := map[string]bool{}
	local := localAddrs()
	for _, p := range peers {
		pub, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			m.log.Warn("ignoring mesh peer with an invalid key", "node", p.Node)
			continue
		}
		ep, err := net.ResolveUDPAddr("udp", p.Endpoint)
		if err != nil {
			m.log.Warn("ignoring mesh peer with an invalid endpoint", "node", p.Node, "endpoint", p.Endpoint)
			continue
		}
		_, cidr, err := net.ParseCIDR(p.PodCIDR)
		if err != nil {
			continue
		}
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{
			PublicKey: pub, Endpoint: ep, AllowedIPs: []net.IPNet{*cidr},
			PersistentKeepaliveInterval: &ka, ReplaceAllowedIPs: true,
		})
		// A peer on this very host (several agents on one machine) is
		// reachable directly over its bridge: routing it into the tunnel
		// would only loop.
		if !local[ep.IP.String()] {
			want[cidr.String()] = true
		}
	}
	wc, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("WireGuard control: %w", err)
	}
	defer wc.Close()
	if err := wc.ConfigureDevice(m.iface, cfg); err != nil {
		return fmt.Errorf("configure WireGuard: %w", err)
	}
	return m.syncRoutes(ctx, want)
}

// localAddrs returns this host's IP addresses, loopback included.
func localAddrs() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out[n.IP.String()] = true
		}
	}
	return out
}

// syncRoutes routes each peer subnet through the tunnel and removes routes
// of peers that left. It never takes over a route of another device: a
// subnet that is already routed elsewhere is left alone.
func (m *Mesh) syncRoutes(ctx context.Context, want map[string]bool) error {
	out, err := ip(ctx, "-o", "route", "show", "dev", m.iface)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			have[f[0]] = true
		}
	}
	var errs []error
	for cidr := range want {
		if have[cidr] {
			continue
		}
		if other, _ := ip(ctx, "-o", "route", "show", "exact", cidr); len(strings.TrimSpace(string(other))) > 0 {
			m.log.Warn("not routing a peer subnet through the mesh: it is already routed", "subnet", cidr, "route", strings.TrimSpace(string(other)))
			continue
		}
		if _, err := ip(ctx, "route", "add", cidr, "dev", m.iface); err != nil {
			errs = append(errs, err)
		}
	}
	for cidr := range have {
		if !want[cidr] {
			if _, err := ip(ctx, "route", "delete", cidr, "dev", m.iface); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
