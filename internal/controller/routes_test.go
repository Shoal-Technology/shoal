package controller

import (
	"testing"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/store"
)

func (e *env) routeRec(name, svc string, port int32, tls, secret string) {
	e.t.Helper()
	spec, err := objects.Encode(&v1alpha1.RouteSpec{Host: name + ".example.com", PathPrefix: "/", Service: svc, Port: port, Tls: tls, TlsSecret: secret})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindRoute, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) routeStatus(name string) *v1alpha1.RouteStatus {
	e.t.Helper()
	rec, err := e.st.Get(ctx, v1alpha1.KindRoute, name)
	if err != nil {
		e.t.Fatal(err)
	}
	rt, err := objects.Route(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	return rt.Status
}

// The controller writes each Route's status on every pass: Ready when the
// Service has endpoints and the Secret parses, otherwise the first reason.
func TestRouteStatusIsWrittenByReconcile(t *testing.T) {
	e, _ := serviceFixture(t, "Replace")
	e.service("idle", "nobody") // a Service whose selector matches nothing

	bad, err := objects.Encode(&v1alpha1.SecretSpec{Data: map[string]string{resource.TLSCertKey: "not pem"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindSecret, Name: "bad", Spec: bad}); err != nil {
		t.Fatal(err)
	}

	e.routeRec("ok", "web", 80, "none", "")
	e.routeRec("nosvc", "nope", 80, "none", "")
	e.routeRec("noport", "web", 9999, "none", "")
	e.routeRec("idle", "idle", 80, "none", "")
	e.routeRec("nosecret", "web", 80, "secret", "absent")
	e.routeRec("badsecret", "web", 80, "secret", "bad")
	e.reconcile()

	want := map[string]string{
		"ok":        resource.RouteReady,
		"nosvc":     resource.RouteServiceNotFound,
		"noport":    resource.RoutePortNotFound,
		"idle":      resource.RouteNoEndpoints,
		"nosecret":  resource.RouteSecretNotFound,
		"badsecret": resource.RouteSecretInvalid,
	}
	for name, state := range want {
		st := e.routeStatus(name)
		if st.GetState() != state {
			t.Errorf("route %s: state %q (%s), want %q", name, st.GetState(), st.GetMessage(), state)
		}
		if (state == resource.RouteReady) != (st.GetMessage() == "") {
			t.Errorf("route %s: message %q for state %q", name, st.GetMessage(), state)
		}
	}

	// A second pass over the same state writes nothing new.
	rec, _ := e.st.Get(ctx, v1alpha1.KindRoute, "ok")
	before := rec.Revision
	e.reconcile()
	rec, _ = e.st.Get(ctx, v1alpha1.KindRoute, "ok")
	if rec.Revision != before {
		t.Fatalf("unchanged route status was rewritten (revision %d -> %d)", before, rec.Revision)
	}
}
