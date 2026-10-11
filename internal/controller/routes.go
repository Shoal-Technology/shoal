package controller

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// Route certificate states (RouteStatus.cert_state).
const (
	CertPending = "CertPending"
	CertIssued  = "CertIssued"
	CertError   = "CertError"
)

// syncRouteStatus sets each Route's status from its Service, its Secret (tls:
// secret) and its Certificate (tls: auto; ADR 0015, passes B2 and C1). The
// controller is the only writer of Route status. Only changes are written.
func (c *Controller) syncRouteStatus(ctx context.Context) []error {
	rrecs, err := c.st.List(ctx, v1alpha1.KindRoute)
	if err != nil {
		return []error{err}
	}
	srecs, err := c.st.List(ctx, v1alpha1.KindService)
	if err != nil {
		return []error{err}
	}
	secrecs, err := c.st.List(ctx, v1alpha1.KindSecret)
	if err != nil {
		return []error{err}
	}
	certrecs, err := c.st.List(ctx, v1alpha1.KindCertificate)
	if err != nil {
		return []error{err}
	}
	svcs := map[string]*v1alpha1.Service{}
	for _, r := range srecs {
		svc, err := objects.Service(r)
		if err != nil {
			return []error{err}
		}
		svcs[r.Name] = svc
	}
	secrets := map[string]map[string]string{}
	for _, r := range secrecs {
		sec, err := objects.Secret(r)
		if err != nil {
			return []error{err}
		}
		secrets[r.Name] = sec.Spec.GetData()
	}
	// Claims map each host and path prefix to the Route that holds it. A
	// Route that is not the holder is in Conflict (ADR 0015, pass E).
	claimrecs, err := c.st.List(ctx, v1alpha1.KindRouteClaim)
	if err != nil {
		return []error{err}
	}
	claims := map[string]string{}
	for _, r := range claimrecs {
		claims[r.Name] = string(r.Spec)
	}
	certs := map[string]*v1alpha1.Certificate{}
	for _, r := range certrecs {
		cert, err := objects.Certificate(r)
		if err != nil {
			return []error{err}
		}
		certs[r.Name] = cert
	}

	var errs []error
	for _, r := range rrecs {
		rt, err := objects.Route(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		data, found := secrets[rt.Spec.GetTlsSecret()]
		state, msg := resource.RouteStatusFor(rt, svcs[rt.Spec.GetService()], data, found)
		if owner, held := claims[resource.RouteKey(rt.Spec)]; held && owner != r.Name {
			state, msg = resource.RouteConflict, fmt.Sprintf("host and pathPrefix are already used by route %q", owner)
		}
		st := &v1alpha1.RouteStatus{State: state, Message: msg}
		if rt.Spec.GetTls() == "auto" {
			st.CertState, st.CertMessage, st.CertNotAfter = certStatus(certs[rt.Spec.GetHost()])
		}
		if proto.Equal(rt.Status, st) {
			continue
		}
		b, err := objects.Encode(st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindRoute, r.Name, b); err != nil {
			errs = append(errs, fmt.Errorf("update status of route %s: %w", r.Name, err))
		}
	}
	return errs
}

// certStatus maps a host's Certificate record to the Route's cert fields. A
// certificate that exists but failed its last renewal is CertError and keeps
// its expiry, so the operator sees both.
func certStatus(cert *v1alpha1.Certificate) (state, msg string, notAfter *timestamppb.Timestamp) {
	if cert == nil {
		return CertPending, "", nil
	}
	if cert.Spec.GetChainPem() == "" {
		if e := cert.Status.GetLastError(); e != "" {
			return CertError, e, nil
		}
		return CertPending, "", nil
	}
	notAfter = cert.Status.GetNotAfter()
	if e := cert.Status.GetLastError(); e != "" {
		return CertError, e, notAfter
	}
	return CertIssued, "", notAfter
}
