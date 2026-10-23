package resource

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// testKeyPair returns a self-signed certificate and its key, both PEM.
func testKeyPair(t *testing.T, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestValidateSecret(t *testing.T) {
	cert, key := testKeyPair(t, "app.example.com")
	otherCert, _ := testKeyPair(t, "other.example.com")
	dockerOK := `{"auths":{"ghcr.io":{"auth":"` + b64("bot:tok") + `"}}}`
	tests := []struct {
		name    string
		spec    *v1alpha1.SecretSpec
		wantErr string // substring; empty means valid
	}{
		{"opaque default", &v1alpha1.SecretSpec{Data: map[string]string{"k": "v"}}, ""},
		{"opaque explicit", &v1alpha1.SecretSpec{Type: SecretTypeOpaque, Data: map[string]string{"k": "v"}}, ""},
		{"opaque bad key", &v1alpha1.SecretSpec{Data: map[string]string{"bad key": "v"}}, "invalid key"},
		{"dockerconfigjson ok", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: dockerOK}}, ""},
		{"dockerconfigjson missing key", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{"x": "y"}}, ".dockerconfigjson"},
		{"dockerconfigjson not json", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: "{nope"}}, "not a docker config"},
		{"dockerconfigjson no auths", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: `{}`}}, "lists no registry"},
		{"dockerconfigjson bad base64", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: `{"auths":{"ghcr.io":{"auth":"!!!"}}}`}}, "not base64"},
		{"dockerconfigjson no colon", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: `{"auths":{"ghcr.io":{"auth":"` + b64("nocolon") + `"}}}`}}, "user:password"},
		{"dockerconfigjson empty password", &v1alpha1.SecretSpec{Type: SecretTypeDockerConfigJSON, Data: map[string]string{DockerConfigKey: `{"auths":{"ghcr.io":{"auth":"` + b64("bot:") + `"}}}`}}, "give auth"},
		{"tls ok", &v1alpha1.SecretSpec{Type: SecretTypeTLS, Data: map[string]string{TLSCertKey: cert, TLSKeyKey: key}}, ""},
		{"tls key does not match", &v1alpha1.SecretSpec{Type: SecretTypeTLS, Data: map[string]string{TLSCertKey: otherCert, TLSKeyKey: key}}, "do not form a valid key pair"},
		{"tls missing key", &v1alpha1.SecretSpec{Type: SecretTypeTLS, Data: map[string]string{TLSCertKey: cert}}, "must hold tls.crt and tls.key"},
		{"unknown type", &v1alpha1.SecretSpec{Type: "example.com/token", Data: map[string]string{"k": "v"}}, "is not supported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSecret("creds", tc.spec)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseDockerConfigHostsAndForms(t *testing.T) {
	cfg := `{"auths":{
		"https://index.docker.io/v1/": {"auth":"` + b64("hubuser:hubpass") + `"},
		"ghcr.io": {"username":"bot","password":"tok"},
		"registry.example.com:5000": {"auth":"` + b64("u:p") + `"}
	}}`
	got, err := ParseDockerConfig(map[string]string{DockerConfigKey: cfg})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ host, user, pass string }{
		{"ghcr.io", "bot", "tok"},
		{"registry-1.docker.io", "hubuser", "hubpass"},
		{"registry.example.com:5000", "u", "p"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d credentials, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].GetHost() != w.host || got[i].GetUsername() != w.user || got[i].GetPassword() != w.pass {
			t.Errorf("credential %d = %v, want %v", i, got[i], w)
		}
	}
}

func TestParseDockerConfigDuplicateRegistry(t *testing.T) {
	cfg := `{"auths":{"docker.io":{"auth":"` + b64("a:b") + `"},"https://index.docker.io/v1/":{"auth":"` + b64("a:b") + `"}}}`
	if _, err := ParseDockerConfig(map[string]string{DockerConfigKey: cfg}); err == nil || !strings.Contains(err.Error(), "same registry") {
		t.Fatalf("error %v, want a same-registry error", err)
	}
}

func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io":                   "registry-1.docker.io",
		"https://index.docker.io/v1/": "registry-1.docker.io",
		"ghcr.io/":                    "ghcr.io",
		"http://localhost:5000":       "localhost:5000",
	} {
		if got := RegistryHost(in); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateWorkloadPullSecrets(t *testing.T) {
	// prepare applies the defaults before validating; do the same here.
	validated := func(w *v1alpha1.Workload) error {
		DefaultWorkload(w)
		return ValidateWorkload(w)
	}
	mk := func(names ...string) *v1alpha1.Workload {
		return &v1alpha1.Workload{
			Metadata: &v1alpha1.ObjectMeta{Name: "web"},
			Spec:     &v1alpha1.WorkloadSpec{Image: "ghcr.io/acme/web:1", ImagePullSecrets: names},
		}
	}
	if err := validated(mk("ghcr-creds")); err != nil {
		t.Fatalf("valid pull secret rejected: %v", err)
	}
	if err := validated(mk("Bad_Name")); err == nil || !strings.Contains(err.Error(), "spec.imagePullSecrets[0]") {
		t.Fatalf("error %v, want an imagePullSecrets[0] error", err)
	}
	if err := validated(mk("a", "a")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error %v, want a duplicate error", err)
	}
}

func TestValidDataKey(t *testing.T) {
	for _, k := range []string{"webhook-secret", "tls.crt", "a_b", "-", "x", strings.Repeat("k", 253)} {
		if !ValidDataKey(k) {
			t.Errorf("ValidDataKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", ".", "..", "bad key", "a/b", "a=b", "a+b", strings.Repeat("k", 254)} {
		if ValidDataKey(k) {
			t.Errorf("ValidDataKey(%q) = true, want false", k)
		}
	}
}

// The server accepts the webhook Secret key that the README and the CLI use.
func TestSecretHyphenatedKeyServerSide(t *testing.T) {
	if err := validateData("hook-secret", map[string]string{"webhook-secret": "v"}); err != nil {
		t.Fatalf("webhook-secret rejected: %v", err)
	}
	if err := validateData("hook-secret", map[string]string{"..": "v"}); err == nil {
		t.Error(`key ".." accepted`)
	}
}
