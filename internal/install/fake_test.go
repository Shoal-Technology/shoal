package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// fakeHost simulates a fresh Ubuntu node: files live in a map, systemd units
// have an active flag, and the shoal CLI answers like a single-node server.
// Commands that change state update the facts, so a second run sees the
// result of the first (the idempotency test depends on that).
type fakeHost struct {
	facts      Facts
	files      map[string][]byte
	dirs       map[string]bool
	modes      map[string]fs.FileMode
	fetch      map[string][]byte
	active     map[string]bool
	ufwAllowed map[string]bool
	ops        []string
	cmds       []string
	tokenOut   string
	namespaces string
	containers map[string]string
}

func freshFacts() Facts {
	return Facts{
		Root:             true,
		Hostname:         "edge-1",
		OS:               "linux",
		Arch:             "amd64",
		Kernel:           "6.8.0-45-generic",
		SystemdRunning:   true,
		Cgroup2:          true,
		OverlayFS:        true,
		BrNetfilter:      true,
		Sysctl:           map[string]string{},
		Tools:            map[string]bool{"iptables": true, "ip": true},
		LoopControl:      true,
		MemTotalMiB:      966,
		SwapMiB:          0,
		DiskFreeMiB:      40 * 1024,
		PortOwners:       map[int]string{},
		ContainerdSocket: false,
		IPv4:             "203.0.113.10",
	}
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		facts:      freshFacts(),
		files:      map[string][]byte{},
		dirs:       map[string]bool{},
		modes:      map[string]fs.FileMode{},
		fetch:      map[string][]byte{},
		active:     map[string]bool{},
		ufwAllowed: map[string]bool{},
		tokenOut: "Join a node by running this on it (as root):\n\n" +
			"  shoal agent --server 127.0.0.1:7443 --token tok-local --ca-hash sha256:abc123\n\n" +
			"The token is valid until 2026-10-12T10:00:00Z for 1 use(s).\n",
		namespaces: "shoal\n",
		containers: map[string]string{},
	}
}

func (f *fakeHost) op(s string) { f.ops = append(f.ops, s) }

func (f *fakeHost) Facts(context.Context) (Facts, error) { return f.facts, nil }

func (f *fakeHost) ReadFile(p string) ([]byte, error) {
	data, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeHost) Exists(p string) bool {
	_, ok := f.files[p]
	return ok || f.dirs[p]
}

func (f *fakeHost) WriteFile(p string, data []byte, mode fs.FileMode) error {
	f.files[p] = append([]byte(nil), data...)
	f.modes[p] = mode
	f.op("write " + p)
	if p == pathContainerd {
		f.facts.ContainerdVersion = ContainerdVersion
	}
	if p == pathRunc {
		f.facts.Tools["runc"] = true
	}
	return nil
}

func (f *fakeHost) Remove(p string) error {
	delete(f.files, p)
	f.op("remove " + p)
	return nil
}

func (f *fakeHost) RemoveAll(p string) error {
	for k := range f.files {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(f.files, k)
		}
	}
	for k := range f.dirs {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(f.dirs, k)
		}
	}
	f.op("removeall " + p)
	return nil
}

func (f *fakeHost) MkdirAll(p string, _ fs.FileMode) error {
	f.dirs[p] = true
	f.op("mkdir " + p)
	return nil
}

func (f *fakeHost) Chown(p, user string) error {
	f.op("chown " + p + " " + user)
	return nil
}

func (f *fakeHost) Fetch(_ context.Context, url string) ([]byte, error) {
	data, ok := f.fetch[url]
	if !ok {
		return nil, fmt.Errorf("fake host: no asset for %s", url)
	}
	f.op("fetch " + url)
	return data, nil
}

func (f *fakeHost) start(unit string) {
	f.active[unit] = true
	switch unit {
	case unitContainerd:
		f.facts.ContainerdSocket = true
	case unitServer:
		if _, ok := f.files[pathAdmin]; !ok {
			f.files[pathAdmin] = []byte("admin-config-for-test\n")
		}
	case unitAgent:
		f.files[pathCreds] = []byte("credentials-for-test\n")
	}
}

func (f *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	f.cmds = append(f.cmds, strings.Join(append([]string{name}, args...), " "))
	switch {
	case name == "systemctl":
		unit := args[len(args)-1]
		switch args[0] {
		case "is-active":
			if f.active[unit] {
				return "active\n", nil
			}
			return "", errors.New("inactive")
		case "daemon-reload":
			f.op("daemon-reload")
		case "enable":
			f.op("enable " + unit)
		case "start", "restart":
			f.op(args[0] + " " + unit)
			f.start(unit)
		case "disable":
			f.op("disable " + unit)
			f.active[unit] = false
		default:
			return "", fmt.Errorf("fake systemctl: %v", args)
		}
		return "", nil
	case name == "modprobe":
		if args[0] == "-n" {
			return "", nil
		}
		f.facts.BrNetfilterLoaded = true
		f.op("modprobe " + args[0])
		return "", nil
	case name == "sysctl":
		k, v, _ := strings.Cut(args[1], "=")
		f.facts.Sysctl[k] = v
		f.op("sysctl " + args[1])
		return "", nil
	case name == "containerd" && len(args) == 1 && args[0] == "--version":
		if f.facts.ContainerdVersion == "" {
			return "", errors.New("not found")
		}
		return "containerd containerd.io " + f.facts.ContainerdVersion + " abc\n", nil
	case name == pathShoalBin:
		return f.shoal(args)
	case name == "fallocate", name == "chmod", name == "mkswap":
		f.op(name + " " + strings.Join(args, " "))
		return "", nil
	case name == "swapon":
		f.facts.SwapMiB = 1024
		f.op("swapon")
		return "", nil
	case name == "ufw":
		if args[0] == "status" {
			out := "Status: active\n"
			for p := range f.ufwAllowed {
				out += p + "  ALLOW  Anywhere\n"
			}
			return out, nil
		}
		f.ufwAllowed[args[1]] = true
		f.op("ufw allow " + args[1])
		return "", nil
	case name == "ctr":
		if contains(args, "namespaces") {
			return f.namespaces, nil
		}
		for i, a := range args {
			if a == "-n" {
				return f.containers[args[i+1]], nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("fake host: unexpected command %s %v", name, args)
}

func (f *fakeHost) shoal(args []string) (string, error) {
	if !f.active[unitServer] {
		return "", errors.New("server is not running")
	}
	switch {
	case args[0] == "token":
		f.op("token create")
		return f.tokenOut, nil
	case args[0] == "get":
		out := "NAME\tSTATUS\n"
		if f.active[unitAgent] {
			out += "edge-1\tReady\n"
		}
		return out, nil
	case args[0] == "label":
		f.op("label " + strings.Join(args[1:], " "))
		return "", nil
	}
	return "", fmt.Errorf("fake shoal: unexpected %v", args)
}

// tarGz builds a gzip'd tar archive with the given files (name -> data, mode).
func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name string
	mode int64
	data []byte
}

// pinAssets replaces the pinned hashes with the hashes of small fake archives
// for the amd64 assets, and restores the pins when the test ends. The fake
// fetch map serves the archives at the pinned URLs.
func pinAssets(t *testing.T, f *fakeHost) {
	t.Helper()
	oldC, oldR, oldN := containerdAssets["amd64"], runcAssets["amd64"], cniAssets["amd64"]
	t.Cleanup(func() {
		containerdAssets["amd64"], runcAssets["amd64"], cniAssets["amd64"] = oldC, oldR, oldN
	})
	cdata := tarGz(t, []tarEntry{
		{"bin/containerd", 0o755, []byte("containerd-bin")},
		{"bin/ctr", 0o755, []byte("ctr-bin")},
		{"bin/containerd-shim-runc-v2", 0o755, []byte("shim-bin")},
		{"README.md", 0o644, []byte("not installed")},
	})
	rdata := []byte("runc-bin")
	ndata := tarGz(t, []tarEntry{
		{"./bridge", 0o755, []byte("bridge-bin")},
		{"./host-local", 0o755, []byte("host-local-bin")},
		{"./portmap", 0o755, []byte("portmap-bin")},
		{"./firewall", 0o755, []byte("firewall-bin")},
		{"./loopback", 0o755, []byte("loopback-bin")},
		{"./LICENSE", 0o644, []byte("license")},
	})
	containerdAssets["amd64"] = Asset{URL: containerdAssets["amd64"].URL, SHA256: sha256Hex(cdata)}
	runcAssets["amd64"] = Asset{URL: runcAssets["amd64"].URL, SHA256: sha256Hex(rdata)}
	cniAssets["amd64"] = Asset{URL: cniAssets["amd64"].URL, SHA256: sha256Hex(ndata)}
	f.fetch[containerdAssets["amd64"].URL] = cdata
	f.fetch[runcAssets["amd64"].URL] = rdata
	f.fetch[cniAssets["amd64"].URL] = ndata
}

func errorsIsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
