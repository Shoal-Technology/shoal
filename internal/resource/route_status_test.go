package resource

import (
	"slices"
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/edge"
)

func testService(ips ...string) *v1alpha1.Service {
	return &v1alpha1.Service{
		Spec: &v1alpha1.ServiceSpec{Ports: []*v1alpha1.ServicePort{
			{Port: 80, TargetPort: 8080},
			{Port: 53, TargetPort: 53, Protocol: "udp"},
			{Port: 9000},
		}},
		Status: &v1alpha1.ServiceStatus{Endpoints: ips},
	}
}

func testRoute(port int32, tls, secret string) *v1alpha1.Route {
	return &v1alpha1.Route{Spec: &v1alpha1.RouteSpec{Host: "app.example.com", PathPrefix: "/", Service: "web", Port: port, Tls: tls, TlsSecret: secret}}
}

func TestRouteEndpoints(t *testing.T) {
	svc := testService("10.42.0.2", "10.42.1.2")
	got, ok := RouteEndpoints(svc, 80)
	if !ok || !slices.Equal(got, []string{"10.42.0.2:8080", "10.42.1.2:8080"}) {
		t.Fatalf("port 80 = %v, %v; want the ready IPs at target port 8080", got, ok)
	}
	// A port without targetPort uses the service port as the target.
	if got, ok := RouteEndpoints(svc, 9000); !ok || !slices.Equal(got, []string{"10.42.0.2:9000", "10.42.1.2:9000"}) {
		t.Fatalf("port 9000 = %v, %v", got, ok)
	}
	// UDP ports are not used by Routes.
	if _, ok := RouteEndpoints(svc, 53); ok {
		t.Fatal("udp port 53 matched")
	}
	if _, ok := RouteEndpoints(svc, 81); ok {
		t.Fatal("unknown port matched")
	}
	if got, ok := RouteEndpoints(nil, 80); ok || got != nil {
		t.Fatalf("nil service = %v, %v", got, ok)
	}
	if got, ok := RouteEndpoints(testService(), 80); !ok || len(got) != 0 {
		t.Fatalf("no endpoints = %v, %v; want the port found and no endpoints", got, ok)
	}
}

func TestRouteStatusFor(t *testing.T) {
	certPEM, keyPEM := testPair(t)
	good := map[string]string{TLSCertKey: string(certPEM), TLSKeyKey: string(keyPEM)}
	cases := []struct {
		name        string
		route       *v1alpha1.Route
		svc         *v1alpha1.Service
		secret      map[string]string
		secretFound bool
		state       string
	}{
		{"ready", testRoute(80, "none", ""), testService("10.42.0.2"), nil, false, RouteReady},
		{"ready with secret", testRoute(80, "secret", "web-cert"), testService("10.42.0.2"), good, true, RouteReady},
		{"service missing", testRoute(80, "none", ""), nil, nil, false, RouteServiceNotFound},
		{"port missing", testRoute(81, "none", ""), testService("10.42.0.2"), nil, false, RoutePortNotFound},
		{"no endpoints", testRoute(80, "none", ""), testService(), nil, false, RouteNoEndpoints},
		{"secret missing", testRoute(80, "secret", "web-cert"), testService("10.42.0.2"), nil, false, RouteSecretNotFound},
		{"secret without key", testRoute(80, "secret", "web-cert"), testService("10.42.0.2"), map[string]string{TLSCertKey: "x"}, true, RouteSecretInvalid},
		{"secret not a pair", testRoute(80, "secret", "web-cert"), testService("10.42.0.2"), map[string]string{TLSCertKey: "x", TLSKeyKey: "y"}, true, RouteSecretInvalid},
		// A missing Service outranks a bad Secret: the first reason wins.
		{"service before secret", testRoute(80, "secret", "web-cert"), nil, map[string]string{}, true, RouteServiceNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, msg := RouteStatusFor(c.route, c.svc, c.secret, c.secretFound)
			if state != c.state {
				t.Fatalf("state = %q (%s), want %q", state, msg, c.state)
			}
			if (state == RouteReady) != (msg == "") {
				t.Fatalf("message %q should be empty only when Ready", msg)
			}
		})
	}
}

// testPair returns a PEM certificate and key for a test host.
func testPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	c, err := edge.SelfSigned("app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err = edge.EncodePEM(c)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM
}
