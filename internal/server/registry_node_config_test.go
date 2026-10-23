package server

import (
	"net"
	"path/filepath"
	"testing"
)

// TestNodeConfigAdvertisesRegistryOnlyWhenEnabled checks what an agent learns
// about the embedded registry (ADR 0020, pass B): nothing when the registry is
// off, and the advertised host with the listener's port when it is on.
func TestNodeConfigAdvertisesRegistryOnlyWhenEnabled(t *testing.T) {
	ctx := t.Context()

	off := startServer(t, t.TempDir(), &clock{})
	a, err := off.newAgent(t, off.token(t, off.admin(t), 1), "n-off", filepath.Join(t.TempDir(), "c.conf"))
	if err != nil {
		t.Fatal(err)
	}
	nc, err := a.NodeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if nc.GetRegistryAddress() != "" {
		t.Fatalf("registry off: RegistryAddress = %q, want empty", nc.GetRegistryAddress())
	}

	on := startServer(t, t.TempDir(), &clock{}, func(c *Config) { c.RegistryListen = ":5000" })
	b, err := on.newAgent(t, on.token(t, on.admin(t), 1), "n-on", filepath.Join(t.TempDir(), "c.conf"))
	if err != nil {
		t.Fatal(err)
	}
	nc, err = b.NodeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(on.addr)
	if err != nil {
		t.Fatal(err)
	}
	if want := net.JoinHostPort(host, "5000"); nc.GetRegistryAddress() != want {
		t.Fatalf("registry on: RegistryAddress = %q, want %q", nc.GetRegistryAddress(), want)
	}
}

func TestRegistryAddress(t *testing.T) {
	for _, tc := range []struct {
		name, advertise, listen, want string
		fail                          bool
	}{
		{name: "off", advertise: "10.0.0.5:8443", listen: "", want: ""},
		{name: "advertised host, listener port", advertise: "10.0.0.5:8443", listen: ":5000", want: "10.0.0.5:5000"},
		{name: "ipv6 host", advertise: "[2001:db8::1]:8443", listen: "[::]:5000", want: "[2001:db8::1]:5000"},
		{name: "listen without port", advertise: "10.0.0.5:8443", listen: "10.0.0.5", fail: true},
		{name: "advertise without port", advertise: "10.0.0.5", listen: ":5000", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := registryAddress(tc.advertise, tc.listen)
			if tc.fail {
				if err == nil {
					t.Fatalf("registryAddress(%q, %q) = %q, want an error", tc.advertise, tc.listen, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("registryAddress(%q, %q) = %q, %v; want %q", tc.advertise, tc.listen, got, err, tc.want)
			}
		})
	}
}
