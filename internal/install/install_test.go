package install

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

func testOptions(out *bytes.Buffer) Options {
	return Options{
		Edge:     true,
		NodeName: "edge-1",
		Binary:   []byte("shoal-binary-v1"),
		UserName: "alice",
		UserHome: "/home/alice",
		Out:      out,
	}
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want *ExitError, got %v", err)
	}
	return ee.Code
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("dry-run output differs from %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func TestDryRunGolden(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *fakeHost)
		adjust func(o *Options)
	}{
		{name: "fresh", setup: func(f *fakeHost) {}},
		{name: "existing-containerd", setup: func(f *fakeHost) {
			f.facts.ContainerdVersion = "2.3.6"
			f.facts.ContainerdSocket = true
			f.facts.Tools["runc"] = true
		}},
		{name: "join", adjust: func(o *Options) {
			o.Edge = false
			o.Join = &Bundle{Format: BundleFormat, Server: "203.0.113.10:7443", Token: "tok-join", CAHash: "sha256:abc123"}
		}},
		{name: "email-accept-tos", adjust: func(o *Options) {
			o.Email = "ops@example.com"
			o.AcceptTOS = true
			o.Swap = "1G"
			o.OpenFirewall = true
		}, setup: func(f *fakeHost) {
			f.facts.Tools["ufw"] = true
			f.facts.UFW = "inactive"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHost()
			if tc.setup != nil {
				tc.setup(f)
			}
			var out bytes.Buffer
			o := testOptions(&out)
			o.DryRun = true
			if tc.adjust != nil {
				tc.adjust(&o)
			}
			if err := Init(context.Background(), f, o); err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if len(f.ops) != 0 {
				t.Fatalf("dry run changed the host: %v", f.ops)
			}
			golden(t, "dryrun-"+tc.name, out.String())
		})
	}
}

func TestFirstRunInstallsAndIsIdempotent(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	o := testOptions(&out)
	if err := Init(context.Background(), f, o); err != nil {
		t.Fatalf("first run: %v\n%s", err, out.String())
	}
	for _, p := range []string{pathShoalBin, pathContainerd, pathRunc, dirCNI + "/bridge",
		pathServerUnit, pathAgentUnit, pathContainerdUnit, pathContainerdConfig, pathSysctl, pathModules,
		"/home/alice/.shoal/config", pathRecord} {
		if !f.Exists(p) {
			t.Errorf("first run did not create %s", p)
		}
	}
	if got := f.modes["/home/alice/.shoal/config"]; got != 0o600 {
		t.Errorf("admin config mode %o, want 0600", got)
	}
	if !contains(f.ops, "chown /home/alice/.shoal/config alice") {
		t.Errorf("admin config not handed to alice: %v", f.ops)
	}
	if !strings.Contains(string(f.files[pathAgentUnit]), "--ca-hash sha256:abc123") {
		t.Errorf("agent unit lacks the CA hash:\n%s", f.files[pathAgentUnit])
	}
	if !strings.Contains(string(f.files[pathAgentUnit]), "--edge-http :80 --edge-https :443") {
		t.Errorf("agent unit lacks the edge ports:\n%s", f.files[pathAgentUnit])
	}
	if !regexp.MustCompile(`(?m)^(done|ok)\s+node-ready:`).MatchString(out.String()) {
		t.Errorf("node-ready missing from output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "next steps") {
		t.Errorf("next steps not printed:\n%s", out.String())
	}

	// Second run: every step is ok or skip, and the host is not touched.
	f.ops = nil
	var out2 bytes.Buffer
	o.Out = &out2
	if err := Init(context.Background(), f, o); err != nil {
		t.Fatalf("second run: %v\n%s", err, out2.String())
	}
	if len(f.ops) != 0 {
		t.Errorf("second run changed the host: %v", f.ops)
	}
	for _, line := range strings.Split(strings.TrimSpace(out2.String()), "\n") {
		if strings.Contains(line, " preflight ") {
			continue
		}
		if !regexp.MustCompile(`^(ok|skip)\s`).MatchString(line) && !strings.HasPrefix(line, "next steps") &&
			!strings.HasPrefix(line, "  - ") && line != "" {
			t.Errorf("second run line is not a no-op: %q", line)
		}
	}
}

func TestPreflightFailureChangesNothing(t *testing.T) {
	cases := map[string]func(f *fakeHost){
		"not root":      func(f *fakeHost) { f.facts.Root = false },
		"port 443 held": func(f *fakeHost) { f.facts.PortOwners[443] = "nginx" },
		"containerd 1.7": func(f *fakeHost) {
			f.facts.ContainerdVersion = "1.7.27"
		},
		"low memory": func(f *fakeHost) { f.facts.MemTotalMiB = 512 },
		"no cgroup2": func(f *fakeHost) { f.facts.Cgroup2 = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost()
			pinAssets(t, f)
			mutate(f)
			var out bytes.Buffer
			err := Init(context.Background(), f, testOptions(&out))
			if code := exitCode(t, err); code != 2 {
				t.Fatalf("exit code %d, want 2", code)
			}
			if len(f.ops) != 0 || len(f.files) != 0 {
				t.Fatalf("preflight failure changed the host: ops=%v files=%d", f.ops, len(f.files))
			}
		})
	}
}

func TestExistingContainerdIsReused(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	f.facts.ContainerdVersion = "2.3.6"
	f.facts.ContainerdSocket = true
	f.facts.Tools["runc"] = true
	f.files[pathContainerd] = []byte("their-containerd")
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "reusing containerd 2.3.6") {
		t.Errorf("containerd not reused:\n%s", out.String())
	}
	if string(f.files[pathContainerd]) != "their-containerd" {
		t.Errorf("existing containerd was overwritten")
	}
	if f.Exists(pathContainerdUnit) || f.Exists(pathContainerdConfig) {
		t.Errorf("init wrote a containerd unit or config next to a containerd it did not install")
	}
	rec, _, _ := loadRecord(context.Background(), f)
	if rec.ContainerdOwned {
		t.Errorf("record claims init owns containerd")
	}
}

func TestJoinWritesAgentOnly(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	o := testOptions(&out)
	o.Edge = false
	o.Join = &Bundle{Format: BundleFormat, Server: "203.0.113.10:7443", Token: "tok-join", CAHash: "sha256:abc123"}
	if err := Init(context.Background(), f, o); err != nil {
		t.Fatalf("join: %v\n%s", err, out.String())
	}
	if f.Exists(pathServerUnit) || f.Exists(pathAdmin) || f.Exists("/home/alice/.shoal/config") {
		t.Errorf("join started a server or wrote admin config")
	}
	unit := string(f.files[pathAgentUnit])
	if !strings.Contains(unit, "--server 203.0.113.10:7443") || !strings.Contains(unit, "--ca-hash sha256:abc123") {
		t.Errorf("agent unit does not point at the bundle:\n%s", unit)
	}
	if string(f.files[pathJoinToken]) != "tok-join\n" {
		t.Errorf("join token file = %q", f.files[pathJoinToken])
	}
	if rec, _, _ := loadRecord(context.Background(), f); !rec.Joined {
		t.Errorf("record not marked joined")
	}
}

func TestACMEFlagsNeedBothEmailAndTerms(t *testing.T) {
	both := serverUnit("ops@example.com", true, "", "")
	if !strings.Contains(both, "--acme-email ops@example.com --acme-accept-tos") {
		t.Errorf("email and accept-tos given, no ACME flags:\n%s", both)
	}
	emailOnly := serverUnit("ops@example.com", false, "", "")
	if strings.Contains(emailOnly, "acme") {
		t.Errorf("email without accept-tos must not enable ACME:\n%s", emailOnly)
	}
	none := serverUnit("", false, "", "")
	if strings.Contains(none, "acme") {
		t.Errorf("no email, ACME flags present:\n%s", none)
	}
}

func TestSHA256MismatchAborts(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	// Serve a different archive than the pinned hash.
	f.fetch[containerdAssets["amd64"].URL] = []byte("tampered")
	var out bytes.Buffer
	err := Init(context.Background(), f, testOptions(&out))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256 mismatch, got %v", err)
	}
	if code := exitCode(t, err); code != 3 {
		t.Errorf("exit code %d, want 3 (kernel and binary already changed)", code)
	}
	for _, p := range []string{pathContainerd, pathRunc, dirCNI + "/bridge"} {
		if f.Exists(p) {
			t.Errorf("mismatched download was written to %s", p)
		}
	}
}

func TestUninstallKeepsDataByDefault(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v", err)
	}
	var uout bytes.Buffer
	if err := Uninstall(context.Background(), f, UninstallOptions{UserHome: "/home/alice", Out: &uout}); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, uout.String())
	}
	for _, p := range []string{pathServerUnit, pathAgentUnit, pathShoalBin, pathSysctl, pathModules, pathContainerd, pathRunc} {
		if f.Exists(p) {
			t.Errorf("uninstall left %s", p)
		}
	}
	for _, p := range []string{pathAdmin, pathCreds, pathRecord, "/home/alice/.shoal/config"} {
		if !f.Exists(p) {
			t.Errorf("uninstall removed %s; data and the admin config must stay", p)
		}
	}
	if !strings.Contains(uout.String(), "kept") {
		t.Errorf("no kept lines printed:\n%s", uout.String())
	}
}

func TestUninstallKeepsContainerdThatExistedBeforeInit(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	f.facts.ContainerdVersion = "2.3.6"
	f.facts.ContainerdSocket = true
	f.facts.Tools["runc"] = true
	f.files[pathContainerd] = []byte("their-containerd")
	f.files[pathRunc] = []byte("their-runc")
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Uninstall(context.Background(), f, UninstallOptions{Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if string(f.files[pathContainerd]) != "their-containerd" || string(f.files[pathRunc]) != "their-runc" {
		t.Errorf("uninstall removed a containerd that existed before init")
	}
}

func TestUninstallKeepsContainerdWhileContainersRun(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v", err)
	}
	f.namespaces = "shoal\nworkloads\n"
	f.containers["workloads"] = "c1\n"
	var uout bytes.Buffer
	if err := Uninstall(context.Background(), f, UninstallOptions{Out: &uout}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !f.Exists(pathContainerd) || !f.Exists(pathContainerdUnit) {
		t.Errorf("containerd removed while containers run outside namespace shoal")
	}
	if !strings.Contains(uout.String(), "workloads") {
		t.Errorf("busy namespace not reported:\n%s", uout.String())
	}
}

func TestUninstallPurgeNeedsYes(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Uninstall(context.Background(), f, UninstallOptions{Purge: true, Out: &bytes.Buffer{}}); err == nil {
		t.Fatal("--purge without --yes must fail")
	}
	if !f.Exists(pathAdmin) {
		t.Fatal("refused purge still removed data")
	}
	if err := Uninstall(context.Background(), f, UninstallOptions{Purge: true, Yes: true, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if f.Exists(pathAdmin) || f.Exists(pathRecord) {
		t.Errorf("purge kept /var/lib/shoal")
	}
}

func TestUninstallWithoutRecordIsAnError(t *testing.T) {
	f := newFakeHost()
	err := Uninstall(context.Background(), f, UninstallOptions{Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "no install record") {
		t.Fatalf("want a no-record error, got %v", err)
	}
}

func TestSwapAndFirewallAreOptIn(t *testing.T) {
	f := newFakeHost()
	pinAssets(t, f)
	var out bytes.Buffer
	if err := Init(context.Background(), f, testOptions(&out)); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, op := range f.ops {
		if strings.Contains(op, "swapon") || strings.Contains(op, "ufw") {
			t.Errorf("default run touched swap or firewall: %s", op)
		}
	}
	if !regexp.MustCompile(`(?m)^skip\s+swap:`).MatchString(out.String()) || !regexp.MustCompile(`(?m)^skip\s+firewall:`).MatchString(out.String()) {
		t.Errorf("swap and firewall should be skipped by default:\n%s", out.String())
	}
}

func TestParseHelpers(t *testing.T) {
	for in, want := range map[string]uint64{"1G": 1024, "512M": 512, "2g": 2048} {
		got, err := parseSizeMiB(in)
		if err != nil || got != want {
			t.Errorf("parseSizeMiB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1T", "G", "10"} {
		if _, err := parseSizeMiB(bad); err == nil {
			t.Errorf("parseSizeMiB(%q) accepted", bad)
		}
	}
	if _, err := ParseBundle([]byte(`{"format":"shoal-join/v2","server":"a","token":"t","ca_hash":"h"}`)); err == nil {
		t.Error("bundle with the wrong format accepted")
	}
	if _, err := ParseBundle([]byte(`{"format":"shoal-join/v1","server":"a","token":"t"}`)); err == nil {
		t.Error("bundle without ca_hash accepted")
	}
	if maj, minor, ok := kernelVersion("6.8.0-45-generic"); !ok || maj != 6 || minor != 8 {
		t.Errorf("kernelVersion = %d.%d %v", maj, minor, ok)
	}
	if _, _, ok := kernelVersion("garbage"); ok {
		t.Error("kernelVersion accepted garbage")
	}
}
