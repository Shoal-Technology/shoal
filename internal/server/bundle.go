package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// bundleFormat names the JSON layout of a join bundle.
const bundleFormat = "shoal-join-bundle/v1"

// joinBundle is what a new server needs to share the cluster's PKI and
// secrets. It holds the CA private key and the secret encryption key, so it
// is a cluster secret: whoever holds it can issue certificates for the
// cluster and decrypt its Secrets.
type joinBundle struct {
	Format string `json:"format"`
	// CAHash is security.CAHash of CACert. Installing checks it, so a
	// damaged bundle is caught before it reaches the data dir.
	CAHash    string `json:"caHash"`
	CACert    []byte `json:"caCert"`    // pki/ca.crt, PEM
	CAKey     []byte `json:"caKey"`     // pki/ca.key, PEM
	SecretKey []byte `json:"secretKey"` // the secret encryption key, raw
}

// JoinBundle returns the cluster CA and secret key, packed for
// --raft-join-bundle. Admin only, like every Control call. The operator
// writes the bundle to a file with mode 0600 and copies it to the new server.
func (c *controlService) JoinBundle(ctx context.Context, _ *v1alpha1.JoinBundleRequest) (*v1alpha1.JoinBundleResponse, error) {
	s := c.s
	caCert, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "pki", "ca.crt"))
	if err != nil {
		return nil, apiError(fmt.Errorf("read CA certificate: %w", err))
	}
	caKey, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "pki", "ca.key"))
	if err != nil {
		return nil, apiError(fmt.Errorf("read CA key: %w", err))
	}
	secret, err := os.ReadFile(s.cfg.EncryptionKeyFile)
	if err != nil {
		return nil, apiError(fmt.Errorf("read secret key: %w", err))
	}
	b := joinBundle{
		Format:    bundleFormat,
		CAHash:    security.CAHash(s.ca.Cert),
		CACert:    caCert,
		CAKey:     caKey,
		SecretKey: secret,
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, apiError(err)
	}
	s.log.Warn("join bundle issued", "peer", peerAddr(ctx), "caHash", b.CAHash)
	return &v1alpha1.JoinBundleResponse{Bundle: out, CaHash: b.CAHash}, nil
}

// parseJoinBundle decodes a bundle and checks that its CA certificate is PEM
// and matches its hash, and that the keys are present.
func parseJoinBundle(raw []byte) (joinBundle, error) {
	var b joinBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("not a join bundle: %w", err)
	}
	if b.Format != bundleFormat {
		return b, fmt.Errorf("unsupported bundle format %q (want %q)", b.Format, bundleFormat)
	}
	block, _ := pem.Decode(b.CACert)
	if block == nil || block.Type != "CERTIFICATE" {
		return b, errors.New("CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return b, fmt.Errorf("parse CA certificate: %w", err)
	}
	if security.CAHash(cert) != b.CAHash {
		return b, errors.New("CA hash does not match the CA certificate (damaged bundle)")
	}
	if len(b.CAKey) == 0 || len(b.SecretKey) == 0 {
		return b, errors.New("bundle is missing the CA key or the secret key")
	}
	return b, nil
}

// installJoinBundle writes the bundle's CA certificate, CA key and secret key
// into the data dir before the server starts. It reports whether it wrote
// anything. Each target that already exists must match the bundle byte for
// byte. Otherwise it refuses and writes nothing, so a data dir with another
// cluster's PKI is never overwritten. Running it again with the same bundle
// is a no-op.
func installJoinBundle(path, dataDir, keyFile string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read join bundle: %w", err)
	}
	b, err := parseJoinBundle(raw)
	if err != nil {
		return false, fmt.Errorf("join bundle %s: %w", path, err)
	}
	pkiDir := filepath.Join(dataDir, "pki")
	type target struct {
		path string
		data []byte
	}
	targets := []target{
		{filepath.Join(pkiDir, "ca.crt"), b.CACert},
		{filepath.Join(pkiDir, "ca.key"), b.CAKey},
		{keyFile, b.SecretKey},
	}
	// Check every target before writing any of them.
	var missing []target
	for _, t := range targets {
		cur, err := os.ReadFile(t.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, t)
		case err != nil:
			return false, fmt.Errorf("read %s: %w", t.path, err)
		case bytes.Equal(cur, t.data):
			// Already installed from this bundle.
		default:
			return false, fmt.Errorf("refusing to install join bundle: %s exists and differs from the bundle; "+
				"this data dir holds another cluster's PKI, so use an empty --data-dir", t.path)
		}
	}
	for _, t := range missing {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
			return false, fmt.Errorf("create %s: %w", filepath.Dir(t.path), err)
		}
		if err := writeNewFile(t.path, t.data); err != nil {
			return false, err
		}
	}
	return len(missing) > 0, nil
}

// writeNewFile creates path with mode 0600 and fails if it already exists.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}
