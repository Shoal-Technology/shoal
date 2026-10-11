package controller

import (
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/objects"
	"github.com/kaiserern/shoal/internal/resource"
	"github.com/kaiserern/shoal/internal/store"
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

// A Route that is not the holder of its host and path prefix claim is in
// Conflict; the holder is not (ADR 0015, pass E).
func TestRouteConflictIsSetFromClaims(t *testing.T) {
	e, _ := serviceFixture(t, "Replace")
	spec := &v1alpha1.RouteSpec{Host: "shared.example.com", PathPrefix: "/", Service: "web", Port: 80, Tls: "none"}
	b, err := objects.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new"} {
		if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindRoute, Name: name, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindRouteClaim, Name: resource.RouteKey(spec), Spec: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.routeStatus("new").GetState(); got != resource.RouteConflict {
		t.Fatalf("new route state = %q, want %q", got, resource.RouteConflict)
	}
	if got := e.routeStatus("old").GetState(); got != resource.RouteReady {
		t.Fatalf("holder state = %q, want %q", got, resource.RouteReady)
	}
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
