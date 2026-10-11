package server

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strconv"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
)

// dataSources holds the Secrets and Configs a snapshot resolves from.
type dataSources struct {
	secrets map[string]map[string]string
	configs map[string]map[string]string
}

func (s *Server) loadDataSources(ctx context.Context) (*dataSources, error) {
	d := &dataSources{secrets: map[string]map[string]string{}, configs: map[string]map[string]string{}}
	secs, err := s.store.List(ctx, v1alpha1.KindSecret)
	if err != nil {
		return nil, err
	}
	for _, r := range secs {
		sec, err := objects.Secret(r)
		if err != nil {
			return nil, err
		}
		d.secrets[r.Name] = sec.Spec.GetData()
	}
	cfgs, err := s.store.List(ctx, v1alpha1.KindConfig)
	if err != nil {
		return nil, err
	}
	for _, r := range cfgs {
		cfg, err := objects.Config(r)
		if err != nil {
			return nil, err
		}
		d.configs[r.Name] = cfg.Spec.GetData()
	}
	return d, nil
}

// resolve fills in an assignment's env and mounts from the template's
// references. A missing Secret, Config or key is reported as the
// assignment's problem; the agent then leaves the replica alone.
func (d *dataSources) resolve(a *v1alpha1.Assignment) {
	t := a.GetTemplate()
	lookup := func(kind string, src map[string]map[string]string, name, key string) (string, bool) {
		data, ok := src[name]
		if !ok {
			a.Problem = fmt.Sprintf("%s %q not found", kind, name)
			return "", false
		}
		v, ok := data[key]
		if !ok {
			a.Problem = fmt.Sprintf("%s %q has no key %q", kind, name, key)
		}
		return v, ok
	}
	for _, e := range t.GetEnv() {
		v := e.GetValue()
		if from := e.GetValueFrom(); from != nil {
			var ok bool
			if ref := from.GetSecret(); ref != nil {
				v, ok = lookup("secret", d.secrets, ref.GetName(), ref.GetKey())
			} else {
				ref := from.GetConfig()
				v, ok = lookup("config", d.configs, ref.GetName(), ref.GetKey())
			}
			if !ok {
				return
			}
		}
		a.Env = append(a.Env, &v1alpha1.EnvVar{Name: e.GetName(), Value: v})
	}
	for _, m := range t.GetFiles() {
		src, kind, name := d.configs, "config", m.GetConfig()
		if m.GetSecret() != "" {
			src, kind, name = d.secrets, "secret", m.GetSecret()
		}
		data, ok := src[name]
		if !ok {
			a.Problem = fmt.Sprintf("%s %q not found", kind, name)
			return
		}
		a.Mounts = append(a.Mounts, &v1alpha1.ResolvedMount{Path: m.GetPath(), Files: maps.Clone(data), Secret: kind == "secret"})
	}
}

// serviceRoutes lists every Service with an IP, for load balancing and DNS
// on the nodes.
func (s *Server) serviceRoutes(ctx context.Context) ([]*v1alpha1.ServiceRoute, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindService)
	if err != nil {
		return nil, err
	}
	var out []*v1alpha1.ServiceRoute
	for _, r := range recs {
		svc, err := objects.Service(r)
		if err != nil {
			return nil, err
		}
		if svc.Status.GetClusterIp() == "" {
			continue
		}
		out = append(out, &v1alpha1.ServiceRoute{
			Name: r.Name, ClusterIp: svc.Status.GetClusterIp(),
			Ports: svc.Spec.GetPorts(), Endpoints: svc.Status.GetEndpoints(),
		})
	}
	return out, nil
}

// meshPeers lists the other nodes that can take part in the WireGuard
// mesh (they have a subnet, a public key and an endpoint).
func (s *Server) meshPeers(ctx context.Context, self string) ([]*v1alpha1.MeshPeer, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindNode)
	if err != nil {
		return nil, err
	}
	var out []*v1alpha1.MeshPeer
	for _, r := range recs {
		if r.Name == self {
			continue
		}
		n, err := objects.Node(r)
		if err != nil {
			return nil, err
		}
		st := n.Status
		if n.Spec.GetPodCidr() == "" || st.GetWireguardPublicKey() == "" || st.GetWireguardEndpoint() == "" {
			continue
		}
		out = append(out, &v1alpha1.MeshPeer{
			Node: r.Name, PublicKey: st.GetWireguardPublicKey(), Endpoint: st.GetWireguardEndpoint(), PodCidr: n.Spec.GetPodCidr(),
		})
	}
	return out, nil
}

// wireguardEndpoint is where peers reach a node: its explicit endpoint, or
// the address the server sees with its WireGuard port.
func wireguardEndpoint(addr string, hb *v1alpha1.Heartbeat) string {
	if e := hb.GetWireguardEndpoint(); e != "" {
		return e
	}
	if hb.GetWireguardPort() == 0 || addr == "" {
		return ""
	}
	return net.JoinHostPort(addr, strconv.Itoa(int(hb.GetWireguardPort())))
}
