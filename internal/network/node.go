package network

import (
	"context"
	"errors"
	"log/slog"
	"net"
)

// Node is the cluster networking of one node: Service load balancing, the
// Service DNS resolver and (optionally) the WireGuard mesh.
type Node struct {
	proxy   *ServiceProxy
	dns     *DNS
	mesh    *Mesh // nil if the mesh is disabled
	dnsAddr string
	log     *slog.Logger
}

// NodeConfig configures a node's networking.
type NodeConfig struct {
	PodCIDR     string
	ClusterCIDR string
	DNSDomain   string
	// WireGuard: the interface name, its UDP port and where the private key
	// is kept. An empty Interface disables the mesh (single-node setups).
	WireguardInterface string
	WireguardPort      int
	WireguardKeyPath   string
	Log                *slog.Logger
}

// NewNode prepares the node's networking; Run and Sync activate it.
func NewNode(cfg NodeConfig) (*Node, error) {
	gw, err := GatewayIP(cfg.PodCIDR)
	if err != nil {
		return nil, err
	}
	n := &Node{
		proxy:   NewServiceProxy(cfg.ClusterCIDR),
		dns:     NewDNS(cfg.DNSDomain, HostResolvers(), cfg.Log),
		dnsAddr: net.JoinHostPort(gw, "53"),
		log:     cfg.Log,
	}
	if cfg.WireguardInterface != "" {
		if n.mesh, err = NewMesh(cfg.WireguardKeyPath, cfg.WireguardInterface, cfg.WireguardPort, cfg.PodCIDR, cfg.Log); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// DNSServer is the address containers use as nameserver.
func (n *Node) DNSServer() string {
	host, _, _ := net.SplitHostPort(n.dnsAddr)
	return host
}

// WireGuard returns the node's public key and port, or zero values when
// the mesh is disabled.
func (n *Node) WireGuard() (publicKey string, port int) {
	if n.mesh == nil {
		return "", 0
	}
	return n.mesh.PublicKey(), n.mesh.Port()
}

// Run serves DNS until ctx is done.
func (n *Node) Run(ctx context.Context) error {
	return n.dns.ListenAndServe(ctx, n.dnsAddr)
}

// Sync applies the cluster's Services and mesh peers. All three parts are
// attempted even if one fails.
func (n *Node) Sync(ctx context.Context, services []Service, peers []Peer) error {
	n.dns.SetServices(services)
	errs := []error{n.proxy.Sync(ctx, services)}
	if n.mesh != nil {
		errs = append(errs, n.mesh.Sync(ctx, peers))
	}
	return errors.Join(errs...)
}
