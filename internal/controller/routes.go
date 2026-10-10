package controller

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
)

// syncRouteStatus sets each Route's status from its Service and, for tls:
// secret, its Secret (ADR 0015, pass B2). The controller is the only writer
// of Route status. Only changes are written.
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

	var errs []error
	for _, r := range rrecs {
		rt, err := objects.Route(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		data, found := secrets[rt.Spec.GetTlsSecret()]
		state, msg := resource.RouteStatusFor(rt, svcs[rt.Spec.GetService()], data, found)
		st := &v1alpha1.RouteStatus{State: state, Message: msg, CertState: rt.Status.GetCertState()}
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
