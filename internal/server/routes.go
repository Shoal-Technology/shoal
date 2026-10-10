package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
)

// routeSnapshot builds the ingress configuration for node (ADR 0015, passes
// B2 and C1). A node without the edge label gets a snapshot with Edge false and
// no routes, so it never receives a Secret or an issued key.
func (s *Server) routeSnapshot(ctx context.Context, node string) (*v1alpha1.RouteSnapshot, error) {
	nrec, err := s.store.Get(ctx, v1alpha1.KindNode, node)
	if err != nil {
		return nil, err
	}
	n, err := objects.Node(nrec)
	if err != nil {
		return nil, err
	}
	snap := &v1alpha1.RouteSnapshot{Edge: n.Spec.GetLabels()[resource.EdgeLabel] == "true"}
	if !snap.Edge {
		return snap, nil
	}

	svcs := map[string]*v1alpha1.Service{}
	srecs, err := s.store.List(ctx, v1alpha1.KindService)
	if err != nil {
		return nil, err
	}
	for _, r := range srecs {
		svc, err := objects.Service(r)
		if err != nil {
			return nil, err
		}
		svcs[r.Name] = svc
	}
	secrets := map[string]map[string]string{}
	secrecs, err := s.store.List(ctx, v1alpha1.KindSecret)
	if err != nil {
		return nil, err
	}
	for _, r := range secrecs {
		sec, err := objects.Secret(r)
		if err != nil {
			return nil, err
		}
		secrets[r.Name] = sec.Spec.GetData()
	}

	rrecs, err := s.store.List(ctx, v1alpha1.KindRoute)
	if err != nil {
		return nil, err
	}
	autoHosts := map[string]bool{}
	for _, r := range rrecs {
		rt, err := objects.Route(r)
		if err != nil {
			return nil, err
		}
		spec := rt.Spec
		er := &v1alpha1.EdgeRoute{
			Name: r.Name, Host: spec.GetHost(), PathPrefix: spec.GetPathPrefix(), Tls: spec.GetTls(),
			RedirectHttp: spec.GetRedirectHttp(),
		}
		// Endpoints come from the Service status, as in the service proxy.
		er.Endpoints, _ = resource.RouteEndpoints(svcs[spec.GetService()], spec.GetPort())
		if spec.GetTls() == "secret" {
			// A Secret that does not parse gets no certificate here; the
			// Route status reports SecretInvalid.
			if cert, key, err := resource.ParseRouteSecret(secrets[spec.GetTlsSecret()]); err == nil {
				er.CertPem, er.KeyPem = cert, key
			}
		}
		if spec.GetTls() == "auto" {
			autoHosts[spec.GetHost()] = true
		}
		snap.Routes = append(snap.Routes, er)
	}
	slices.SortFunc(snap.Routes, func(x, y *v1alpha1.EdgeRoute) int {
		if x.Host != y.Host {
			if x.Host < y.Host {
				return -1
			}
			return 1
		}
		return len(y.PathPrefix) - len(x.PathPrefix)
	})

	// Issued certificates for tls auto hosts. A host with no issued
	// certificate yet keeps the edge's self-signed fallback.
	crecs, err := s.store.List(ctx, v1alpha1.KindCertificate)
	if err != nil {
		return nil, err
	}
	for _, r := range crecs {
		if !autoHosts[r.Name] {
			continue
		}
		c, err := objects.Certificate(r)
		if err != nil {
			return nil, err
		}
		if c.Spec.GetChainPem() == "" {
			continue
		}
		snap.Certificates = append(snap.Certificates, &v1alpha1.EdgeCertificate{
			Host: r.Name, ChainPem: []byte(c.Spec.GetChainPem()), KeyPem: []byte(c.Spec.GetKeyPem()),
		})
	}

	// HTTP-01 tokens that are still live. Expired ones are left out even if
	// the manager did not delete them (for example after a crash).
	now := s.now()
	hrecs, err := s.store.List(ctx, v1alpha1.KindAcmeChallenge)
	if err != nil {
		return nil, err
	}
	for _, r := range hrecs {
		ch, err := objects.AcmeChallenge(r)
		if err != nil {
			return nil, err
		}
		if !ch.Spec.GetExpiresAt().AsTime().After(now) {
			continue
		}
		snap.Challenges = append(snap.Challenges, &v1alpha1.EdgeChallenge{Token: r.Name, KeyAuthorization: ch.Spec.GetKeyAuthorization()})
	}
	return snap, nil
}

// routePusher sends route snapshots on one stream. It sends a snapshot only
// when its content changes (or on a resync), and one disabled snapshot when
// the node loses the edge label. Nodes that were never edges get nothing.
type routePusher struct {
	version  string
	revision int64
	edge     bool
}

// push sends snap if it is needed. The revision comes from the clock and
// always increases on this stream, so an agent can drop stale snapshots.
func (p *routePusher) push(stream v1alpha1.Agent_SyncServer, snap *v1alpha1.RouteSnapshot, now time.Time, force bool) error {
	if !snap.GetEdge() && !p.edge {
		return nil
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(snap)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	version := hex.EncodeToString(sum[:6])
	if !force && version == p.version && snap.GetEdge() == p.edge {
		return nil
	}
	rev := now.UnixNano()
	if rev <= p.revision {
		rev = p.revision + 1
	}
	snap.Revision, snap.Version = rev, version
	if err := stream.Send(&v1alpha1.ServerMessage{Msg: &v1alpha1.ServerMessage_Routes{Routes: snap}}); err != nil {
		return err
	}
	p.version, p.revision, p.edge = version, rev, snap.GetEdge()
	return nil
}
