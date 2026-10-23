package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// cidrAllocator hands out one /24 container subnet per node from the
// cluster CIDR. The mutex makes allocation safe within one server; with
// several servers (Phase 6) the Node update's compare-and-swap is the
// guard.
type cidrAllocator struct {
	cluster  netip.Prefix
	services netip.Prefix
	mu       sync.Mutex
}

func newCIDRAllocator(cluster, services string) (*cidrAllocator, error) {
	c, err := netip.ParsePrefix(cluster)
	if err != nil || !c.Addr().Is4() || c.Bits() > 23 {
		return nil, fmt.Errorf("invalid cluster CIDR %q: want an IPv4 prefix of /23 or larger", cluster)
	}
	s, err := netip.ParsePrefix(services)
	if err != nil || !s.Addr().Is4() || s.Bits() > 28 {
		return nil, fmt.Errorf("invalid service CIDR %q: want an IPv4 prefix of /28 or larger", services)
	}
	c, s = c.Masked(), s.Masked()
	if c.Overlaps(s) {
		return nil, fmt.Errorf("cluster CIDR %s and service CIDR %s overlap", c, s)
	}
	return &cidrAllocator{cluster: c, services: s}, nil
}

// subnet returns the i-th /24 of the cluster CIDR.
func (a *cidrAllocator) subnet(i int) (netip.Prefix, bool) {
	if i >= 1<<(24-a.cluster.Bits()) {
		return netip.Prefix{}, false
	}
	b := a.cluster.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(i) << 8
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), 24), true
}

// podCIDR returns node's container subnet, assigning the lowest free one
// (skipping the first, used by standalone agents) on first use.
func (s *Server) podCIDR(ctx context.Context, node string) (string, error) {
	s.cidrs.mu.Lock()
	defer s.cidrs.mu.Unlock()
	for range casRetries {
		rec, err := s.store.Get(ctx, v1alpha1.KindNode, node)
		if err != nil {
			return "", err
		}
		n, err := objects.Node(rec)
		if err != nil {
			return "", err
		}
		if n.Spec.GetPodCidr() != "" {
			return n.Spec.GetPodCidr(), nil
		}
		recs, err := s.store.List(ctx, v1alpha1.KindNode)
		if err != nil {
			return "", err
		}
		used := map[string]bool{}
		for _, r := range recs {
			if other, err := objects.Node(r); err == nil {
				used[other.Spec.GetPodCidr()] = true
			}
		}
		var cidr string
		for i := 1; ; i++ {
			p, ok := s.cidrs.subnet(i)
			if !ok {
				return "", status.Errorf(codes.ResourceExhausted, "no free /24 left in cluster CIDR %s", s.cidrs.cluster)
			}
			if !used[p.String()] {
				cidr = p.String()
				break
			}
		}
		n.Spec.PodCidr = cidr
		b, err := encode(n.Spec)
		if err != nil {
			return "", err
		}
		_, err = s.store.Update(ctx, v1alpha1.KindNode, node, b, rec.Generation)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return "", err
		}
		s.log.Info("assigned container subnet", "node", node, "podCIDR", cidr)
		return cidr, nil
	}
	return "", status.Error(codes.Aborted, "node changed concurrently, try again")
}

// NodeConfig returns the calling node's network settings and, when the
// embedded registry is on, the address agents pull from (ADR 0020, pass B).
func (a *agentService) NodeConfig(ctx context.Context, _ *v1alpha1.NodeConfigRequest) (*v1alpha1.NodeConfigResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	cidr, err := a.s.podCIDR(ctx, c.id.Name)
	if err != nil {
		return nil, apiError(err)
	}
	regAddr, err := registryAddress(a.s.cfg.AdvertiseAddress, a.s.cfg.RegistryListen)
	if err != nil {
		return nil, apiError(err)
	}
	return &v1alpha1.NodeConfigResponse{
		PodCidr:         cidr,
		ClusterCidr:     a.s.cidrs.cluster.String(),
		DnsDomain:       v1alpha1.DNSDomain,
		RegistryAddress: regAddr,
	}, nil
}

// registryAddress is the host:port agents dial for the embedded registry: the
// host of the advertise address with the port of the registry listener. It is
// empty when the registry is off. The registry certificate names the advertise
// host (servingCert), so the agent's TLS check passes on this address. The
// listener must be reachable at that host from every node.
func registryAddress(advertise, listen string) (string, error) {
	if listen == "" {
		return "", nil
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("registry listen address %q: %w", listen, err)
	}
	host, _, err := net.SplitHostPort(advertise)
	if err != nil {
		return "", fmt.Errorf("advertise address %q: %w", advertise, err)
	}
	return net.JoinHostPort(host, port), nil
}
