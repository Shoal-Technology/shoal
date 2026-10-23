package runtime

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
)

// RegistryAlias is the host name a workload can use for the embedded registry
// on any node: shoal.local/<repo>:<tag> pulls from the registry at the address
// the server advertised (ADR 0020, pass B). Nothing resolves the name; the
// agent rewrites it before it pulls or looks an image up.
const RegistryAlias = "shoal.local"

// Registry is the embedded OCI registry as an agent reaches it (ADR 0020,
// pass B). The zero value is not usable; a nil *Registry means the registry is
// not enabled on this node.
type Registry struct {
	// Address is host:port as the server advertises it (NodeConfig). The
	// registry certificate names the advertised host.
	Address string
	// CA is the cluster CA. It is the only root the agent trusts for the
	// registry, and the agent never skips verification.
	CA *x509.Certificate
	// ClientCertificate returns the node's client certificate. It is called
	// on each handshake, so a renewed certificate is used without a restart.
	ClientCertificate func() (*tls.Certificate, error)
}

// MapImage rewrites a reference that starts with RegistryAlias to the registry
// address, so shoal.local/team/app:v1 becomes <address>/team/app:v1. Other
// references are returned unchanged, and so is every reference when r is nil.
func (r *Registry) MapImage(ref string) string {
	if r == nil {
		return ref
	}
	if rest, ok := strings.CutPrefix(ref, RegistryAlias+"/"); ok {
		return r.Address + "/" + rest
	}
	return ref
}

// TLSConfig returns the client settings for the registry: TLS 1.3, the cluster
// CA as the only root and the node's client certificate (ADR 0007, ADR 0020).
func (r *Registry) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(r.CA)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := r.ClientCertificate()
			if err != nil {
				return nil, err
			}
			if c == nil {
				// A nil certificate would crash the TLS stack; failing the
				// handshake is the right outcome for a node without one.
				return nil, errors.New("registry: node has no client certificate")
			}
			return c, nil
		},
	}
}

// HTTPClient returns an HTTP client for the registry that uses TLSConfig. It
// never goes through a proxy: the registry is an internal address.
func (r *Registry) HTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = r.TLSConfig()
	return &http.Client{Transport: tr}
}
