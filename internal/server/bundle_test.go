package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/security"
)

// testBundleSeed returns a well-formed join bundle with a fresh self-signed CA.
func testBundleSeed(f *testing.F) []byte {
	f.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "shoal test CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(4102444800, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		f.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		f.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		f.Fatal(err)
	}
	b := joinBundle{
		Format:    bundleFormat,
		CAHash:    security.CAHash(cert),
		CACert:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		CAKey:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		SecretKey: []byte("0123456789abcdef0123456789abcdef"),
	}
	out, err := json.Marshal(b)
	if err != nil {
		f.Fatal(err)
	}
	return out
}

// FuzzParseJoinBundle checks that arbitrary bundle bytes never panic the
// parser, and that an accepted bundle has a CA certificate whose hash matches
// and non-empty keys.
func FuzzParseJoinBundle(f *testing.F) {
	f.Add(testBundleSeed(f))
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"format":"shoal-join-bundle/v1"}`))
	f.Add([]byte(`{"format":"shoal-join-bundle/v1","caHash":"x","caCert":"AAAA"}`))
	f.Add([]byte(`{"format":"shoal-join-bundle/v2"}`))
	f.Add([]byte(`{"format":"shoal-join-bundle/v1","caCert":"LS0tLS1CRUdJTi0t"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := parseJoinBundle(data)
		if err != nil {
			return
		}
		block, _ := pem.Decode(b.CACert)
		if block == nil {
			t.Fatal("accepted bundle has no PEM CA certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("accepted bundle has an unparsable CA certificate: %v", err)
		}
		if security.CAHash(cert) != b.CAHash {
			t.Fatal("accepted bundle CA hash does not match its certificate")
		}
		if len(b.CAKey) == 0 || len(b.SecretKey) == 0 {
			t.Fatal("accepted bundle is missing a key")
		}
	})
}
