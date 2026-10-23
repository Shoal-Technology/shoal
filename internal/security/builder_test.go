package security

import (
	"testing"
	"time"
)

func TestBuilderCertificateCarriesScope(t *testing.T) {
	ca, _, err := LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key, "ignored-subject")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignBuilderCSR(csr, Identity{Name: "web-abc12", Role: RoleBuilder, Scope: "web"}, time.Now(), BuildCertificateValidity)
	if err != nil {
		t.Fatal(err)
	}
	id, err := IdentityFromCert(cert)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Identity{Name: "web-abc12", Role: RoleBuilder, Scope: "web"}); id != want {
		t.Fatalf("identity %+v, want %+v", id, want)
	}
	if got := time.Until(cert.NotAfter); got > BuildCertificateValidity+time.Minute {
		t.Fatalf("certificate lives %v, want at most %v", got, BuildCertificateValidity)
	}
}

func TestBuilderCertificateNeedsNameAndScope(t *testing.T) {
	ca, _, err := LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueBuilder(key.Public(), Identity{Name: "web-abc12", Role: RoleBuilder}, time.Now(), time.Hour); err == nil {
		t.Fatal("a builder certificate without a scope was issued")
	}
	if _, err := ca.IssueBuilder(key.Public(), Identity{Scope: "web", Role: RoleBuilder}, time.Now(), time.Hour); err == nil {
		t.Fatal("a builder certificate without a build name was issued")
	}
}
