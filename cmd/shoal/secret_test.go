package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/resource"
)

// writeKeyPair writes a self-signed certificate and key into dir.
func writeKeyPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestBuildSecretOpaque(t *testing.T) {
	obj, err := buildSecret("app-secrets", secretFlags{typ: "opaque"}, []string{"TOKEN=abc", "PASS=-"}, strings.NewReader("hunter2\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := obj.GetSecret().GetSpec()
	if s.GetType() != resource.SecretTypeOpaque || s.GetData()["TOKEN"] != "abc" || s.GetData()["PASS"] != "hunter2" {
		t.Fatalf("spec = %v", s)
	}
}

func TestBuildSecretDockerConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{"auths":{"ghcr.io":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("bot:tok")) + `"}}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	obj, err := buildSecret("ghcr-creds", secretFlags{typ: "dockerconfigjson", fromDockerCfg: path}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := obj.GetSecret().GetSpec()
	if s.GetType() != resource.SecretTypeDockerConfigJSON || s.GetData()[resource.DockerConfigKey] != cfg {
		t.Fatalf("spec = %v", s)
	}
}

func TestBuildSecretDockerConfigFromFlags(t *testing.T) {
	f := secretFlags{typ: "dockerconfigjson", dockerServer: "ghcr.io", dockerUsername: "bot", passwordStdin: true}
	obj, err := buildSecret("ghcr-creds", f, nil, strings.NewReader("tok\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	raw := obj.GetSecret().GetSpec().GetData()[resource.DockerConfigKey]
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(cfg.Auths["ghcr.io"].Auth)
	if err != nil || string(decoded) != "bot:tok" {
		t.Fatalf("auth = %q, %v; want bot:tok", decoded, err)
	}
}

func TestBuildSecretDockerConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		f     secretFlags
		stdin string
		want  string
	}{
		{"no password flag", secretFlags{typ: "dockerconfigjson", dockerServer: "ghcr.io", dockerUsername: "bot"}, "", "give --from-docker-config"},
		{"file and flags", secretFlags{typ: "dockerconfigjson", fromDockerCfg: bad, dockerServer: "ghcr.io"}, "", "cannot be combined"},
		{"colon in user", secretFlags{typ: "dockerconfigjson", dockerServer: "ghcr.io", dockerUsername: "a:b", passwordStdin: true}, "x", "cannot contain"},
		{"invalid file", secretFlags{typ: "dockerconfigjson", fromDockerCfg: bad}, "", "not a docker config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSecret("ghcr-creds", tc.f, nil, strings.NewReader(tc.stdin))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBuildSecretTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeKeyPair(t, dir, "app.example.com")
	obj, err := buildSecret("edge-tls", secretFlags{typ: "tls", cert: cert, key: key}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := obj.GetSecret().GetSpec()
	if s.GetType() != resource.SecretTypeTLS || !strings.Contains(s.GetData()[resource.TLSCertKey], "BEGIN CERTIFICATE") {
		t.Fatalf("spec = %v", s)
	}
	// A key from another pair is refused before any connection.
	_, otherKey := writeKeyPair(t, dir, "other.example.com")
	if _, err := buildSecret("edge-tls", secretFlags{typ: "tls", cert: cert, key: otherKey}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "do not form a valid key pair") {
		t.Fatalf("mismatched key: error %v", err)
	}
}

func TestBuildSecretRejectsMixedFlags(t *testing.T) {
	tests := []struct {
		name string
		f    secretFlags
		args []string
		want string
	}{
		{"unknown type", secretFlags{typ: "token"}, []string{"A=1"}, "unknown --type"},
		{"opaque without values", secretFlags{typ: "opaque"}, nil, "at least one KEY=VALUE"},
		{"opaque with cert", secretFlags{typ: "opaque", cert: "x"}, []string{"A=1"}, "not docker or tls"},
		{"tls with values", secretFlags{typ: "tls", cert: "x", key: "y"}, []string{"A=1"}, "takes --cert and --key"},
		{"tls without key", secretFlags{typ: "tls", cert: "x"}, nil, "needs --cert and --key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSecret("s", tc.f, tc.args, strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The command parses its flags and reads its files before it connects, so a
// bad invocation fails here without a server.
func TestSecretCreateFlagWiring(t *testing.T) {
	cmd := newSecretCmd()
	cmd.SetArgs([]string{"create", "edge-tls", "--type", "tls", "--cert", filepath.Join(t.TempDir(), "missing.crt"), "--key", "k"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing.crt") {
		t.Fatalf("Execute error %v, want the missing certificate file", err)
	}
}

func TestBuildSecretHyphenatedKey(t *testing.T) {
	obj, err := buildSecret("hook-secret", secretFlags{typ: "opaque"}, []string{"webhook-secret=hook-value", "SIGNING=-"}, strings.NewReader("sig\n"))
	if err != nil {
		t.Fatal(err)
	}
	data := obj.GetSecret().GetSpec().GetData()
	if data["webhook-secret"] != "hook-value" || data["SIGNING"] != "sig" {
		t.Errorf("data = %v", data)
	}
	if _, err := buildSecret("hook-secret", secretFlags{typ: "opaque"}, []string{"bad/key=hunter2"}, nil); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("bad key err = %v", err)
	}
}
