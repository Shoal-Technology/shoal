package security

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// ClientConfig holds everything a client needs to talk to the server: the
// CLI's admin.conf and the agent's saved credentials share this format.
type ClientConfig struct {
	Server        string `json:"server"`
	CACertificate string `json:"caCertificate"`
	Certificate   string `json:"certificate"`
	Key           string `json:"key"`
}

// NewClientConfig encodes a certificate and key into a ClientConfig.
func NewClientConfig(server string, ca, cert *x509.Certificate, key crypto.Signer) (*ClientConfig, error) {
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		return nil, err
	}
	return &ClientConfig{
		Server:        server,
		CACertificate: string(EncodeCertPEM(ca)),
		Certificate:   string(EncodeCertPEM(cert)),
		Key:           string(keyPEM),
	}, nil
}

// LoadClientConfig reads a ClientConfig file.
func LoadClientConfig(path string) (*ClientConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}
	var c ClientConfig
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse client config %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config with owner-only permissions.
func (c *ClientConfig) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode client config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return WriteFileAtomic(path, b, 0o600)
}

// Parsed is a decoded ClientConfig.
type Parsed struct {
	CA      *x509.Certificate
	Cert    *x509.Certificate
	TLSCert tls.Certificate
}

// Parse decodes the certificates and key.
func (c *ClientConfig) Parse() (*Parsed, error) {
	ca, err := ParseCertPEM([]byte(c.CACertificate))
	if err != nil {
		return nil, fmt.Errorf("client config CA: %w", err)
	}
	tc, err := tls.X509KeyPair([]byte(c.Certificate), []byte(c.Key))
	if err != nil {
		return nil, fmt.Errorf("client config certificate: %w", err)
	}
	return &Parsed{CA: ca, Cert: tc.Leaf, TLSCert: tc}, nil
}

// TLS returns a client TLS config presenting the config's certificate.
func (p *Parsed) TLS() *tls.Config {
	return ClientTLS(p.CA, func() (*tls.Certificate, error) { return &p.TLSCert, nil })
}
