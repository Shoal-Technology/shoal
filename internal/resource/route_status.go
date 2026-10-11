package resource

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"example.com/shoal/api/v1alpha1"
)

// Route status states (ADR 0015, pass B2). A Route is Ready when it can serve
// traffic now. Otherwise its state is the first reason it cannot, and the
// message says more.
const (
	RouteReady           = "Ready"
	RouteServiceNotFound = "ServiceNotFound"
	RoutePortNotFound    = "PortNotFound"
	RouteNoEndpoints     = "NoEndpoints"
	RouteSecretNotFound  = "SecretNotFound"
	RouteSecretInvalid   = "SecretInvalid"
	// RouteConflict means another Route holds the host and path prefix (its
	// RouteClaim). The controller sets it; see controller.syncRouteStatus.
	RouteConflict = "Conflict"
)

// Keys of the TLS Secret that tls: secret reads (PEM certificate chain and
// private key, the same names Kubernetes uses).
const (
	TLSCertKey = "tls.crt"
	TLSKeyKey  = "tls.key"
)

// EdgeLabel is the node label that makes a node an ingress edge.
const EdgeLabel = "shoal.dev/edge"

// RouteEndpoints returns the endpoints a Route forwards to: the Service's
// Ready replica IPs at the target port of the Service port the Route names,
// as host:port. ok is false when the Service has no TCP port with that number.
// The endpoints come from Service.status, the same source the service proxy uses.
func RouteEndpoints(svc *v1alpha1.Service, port int32) (endpoints []string, ok bool) {
	for _, p := range svc.GetSpec().GetPorts() {
		if p.GetPort() != port {
			continue
		}
		if proto := p.GetProtocol(); proto != "" && proto != "tcp" {
			continue
		}
		target := p.GetTargetPort()
		if target == 0 {
			target = p.GetPort()
		}
		for _, ip := range svc.GetStatus().GetEndpoints() {
			endpoints = append(endpoints, net.JoinHostPort(ip, strconv.Itoa(int(target))))
		}
		return endpoints, true
	}
	return nil, false
}

// ParseRouteSecret reads the PEM certificate and key from a TLS Secret's data
// and checks that they form a usable key pair.
func ParseRouteSecret(data map[string]string) (certPEM, keyPEM []byte, err error) {
	c, okc := data[TLSCertKey]
	k, okk := data[TLSKeyKey]
	if !okc || !okk {
		return nil, nil, fmt.Errorf("the Secret must hold %s and %s", TLSCertKey, TLSKeyKey)
	}
	if _, err := tls.X509KeyPair([]byte(c), []byte(k)); err != nil {
		return nil, nil, fmt.Errorf("%s and %s do not form a valid key pair: %w", TLSCertKey, TLSKeyKey, err)
	}
	return []byte(c), []byte(k), nil
}

// RouteStatusFor decides a Route's status from what it refers to. svc is nil
// when the Service does not exist. secret is nil when the Route does not use
// tls: secret or its Secret does not exist; secretFound says whether the
// Secret exists. Only the state and message are returned.
func RouteStatusFor(rt *v1alpha1.Route, svc *v1alpha1.Service, secret map[string]string, secretFound bool) (state, message string) {
	s := rt.GetSpec()
	if svc == nil {
		return RouteServiceNotFound, fmt.Sprintf("Service %q not found", s.GetService())
	}
	eps, ok := RouteEndpoints(svc, s.GetPort())
	if !ok {
		return RoutePortNotFound, fmt.Sprintf("Service %q has no port %d", s.GetService(), s.GetPort())
	}
	if s.GetTls() == tlsSecret {
		if !secretFound {
			return RouteSecretNotFound, fmt.Sprintf("Secret %q not found", s.GetTlsSecret())
		}
		if _, _, err := ParseRouteSecret(secret); err != nil {
			return RouteSecretInvalid, fmt.Sprintf("Secret %q: %v", s.GetTlsSecret(), err)
		}
	}
	if len(eps) == 0 {
		return RouteNoEndpoints, fmt.Sprintf("Service %q has no ready endpoints", s.GetService())
	}
	return RouteReady, ""
}
