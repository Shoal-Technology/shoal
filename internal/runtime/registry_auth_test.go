package runtime

import (
	"fmt"
	"strings"
	"testing"
)

func TestCredentialsMatchesHost(t *testing.T) {
	auth := []RegistryAuth{{Host: "ghcr.io", Username: "bot", Password: "tok"}}
	if u, p := Credentials(auth, "ghcr.io"); u != "bot" || p != "tok" {
		t.Fatalf("Credentials(ghcr.io) = %q, %q", u, p)
	}
	if u, p := Credentials(auth, "quay.io"); u != "" || p != "" {
		t.Fatalf("Credentials(quay.io) = %q, %q, want anonymous", u, p)
	}
	if u, p := Credentials(nil, "ghcr.io"); u != "" || p != "" {
		t.Fatalf("nil auth gave %q, %q", u, p)
	}
}

func TestRegistryAuthStringOmitsPassword(t *testing.T) {
	s := fmt.Sprintf("%v", RegistryAuth{Host: "ghcr.io", Username: "bot", Password: "s3cret"})
	if strings.Contains(s, "s3cret") {
		t.Fatalf("formatted RegistryAuth leaks the password: %s", s)
	}
}
