//go:build linux

package install

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FakeHostEnv names the environment variable whose value is a JSON file with
// Facts. The file replaces the probes, so that dry-run golden output is stable
// on any machine.
const FakeHostEnv = "SHOAL_INIT_FAKE_HOST"

type localHost struct {
	root string
}

// NewLocalHost returns the Host for this machine. A non-empty root prefixes
// every file path (for tests); commands and probes still run on the real system.
func NewLocalHost(root string) (Host, error) {
	return &localHost{root: root}, nil
}

func (h *localHost) p(path string) string { return filepath.Join(h.root, path) }

func (h *localHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(h.p(path)) }

func (h *localHost) Exists(path string) bool { return fileExists(h.p(path)) }

func (h *localHost) WriteFile(path string, data []byte, mode fs.FileMode) error {
	full := h.p(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".shoal-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (h *localHost) Remove(path string) error {
	err := os.Remove(h.p(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (h *localHost) RemoveAll(path string) error { return os.RemoveAll(h.p(path)) }

func (h *localHost) MkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(h.p(path), mode)
}

func (h *localHost) Chown(path, name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("look up user %s: %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(h.p(path), uid, gid)
}

func (h *localHost) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

func (h *localHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (h *localHost) Facts(ctx context.Context) (Facts, error) {
	if path := os.Getenv(FakeHostEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Facts{}, fmt.Errorf("%s: %w", FakeHostEnv, err)
		}
		var f Facts
		if err := json.Unmarshal(data, &f); err != nil {
			return Facts{}, fmt.Errorf("%s: %w", path, err)
		}
		return f, nil
	}
	f := Facts{
		Root:       os.Geteuid() == 0,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Kernel:     kernelRelease(),
		Tools:      map[string]bool{},
		Sysctl:     map[string]string{},
		PortOwners: map[int]string{},
	}
	f.Hostname, _ = os.Hostname()
	f.SystemdRunning = dirExists("/run/systemd/system")
	f.Cgroup2 = fileExists("/sys/fs/cgroup/cgroup.controllers")
	f.OverlayFS = fileContains("/proc/filesystems", "overlay")
	f.BrNetfilterLoaded = dirExists("/sys/module/br_netfilter")
	_, err := h.Run(ctx, "modprobe", "-n", "br_netfilter")
	f.BrNetfilter = err == nil
	for _, k := range []string{"net.ipv4.ip_forward", "net.bridge.bridge-nf-call-iptables"} {
		if b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(k, ".", "/")); err == nil {
			f.Sysctl[k] = strings.TrimSpace(string(b))
		}
	}
	for _, t := range []string{"iptables", "ip", "runc", "ufw"} {
		_, err := exec.LookPath(t)
		f.Tools[t] = err == nil
	}
	f.LoopControl = fileExists("/dev/loop-control")
	f.MemTotalMiB, f.SwapMiB = memInfo()
	f.DiskFreeMiB = diskFreeMiB("/var/lib")
	for _, port := range []int{80, 443, 7443, 7444} {
		if owner, busy := portOwner(ctx, h, port, "tcp"); busy {
			f.PortOwners[port] = owner
		}
	}
	if owner, busy := portOwner(ctx, h, 51820, "udp"); busy {
		f.PortOwners[51820] = owner
	}
	if _, err := exec.LookPath("containerd"); err == nil {
		out, _ := h.Run(ctx, "containerd", "--version")
		if m := versionRe.FindStringSubmatch(out); m != nil {
			f.ContainerdVersion = m[1]
		}
	}
	f.ContainerdSocket = fileExists(containerdSocket)
	if f.Tools["ufw"] {
		out, _ := h.Run(ctx, "ufw", "status")
		f.UFW = "inactive"
		if strings.Contains(out, "Status: active") {
			f.UFW = "active"
		}
	}
	f.IPv4 = firstIPv4()
	return f, nil
}

var versionRe = regexp.MustCompile(`(?m)\b(\d+\.\d+\.\d+)\b`)

func kernelRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileContains(p, s string) bool {
	b, err := os.ReadFile(p)
	return err == nil && strings.Contains(string(b), s)
}

// memInfo returns MemTotal and SwapTotal in MiB from /proc/meminfo.
func memInfo() (mem, swap uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			mem = kb / 1024
		case "SwapTotal:":
			swap = kb / 1024
		}
	}
	return mem, swap
}

func diskFreeMiB(p string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		if err := syscall.Statfs("/", &st); err != nil {
			return 0
		}
	}
	return st.Bavail * uint64(st.Bsize) / (1 << 20)
}

// portOwner reports whether a port is in use and, when ss can tell, the name
// of the process holding it.
func portOwner(ctx context.Context, h Host, port int, proto string) (string, bool) {
	var ln io.Closer
	var err error
	addr := fmt.Sprintf(":%d", port)
	if proto == "udp" {
		var pc net.PacketConn
		pc, err = net.ListenPacket("udp", addr)
		ln = pc
	} else {
		var l net.Listener
		l, err = net.Listen("tcp", addr)
		ln = l
	}
	if err == nil {
		ln.Close()
		return "", false
	}
	flag := "-ltnp"
	if proto == "udp" {
		flag = "-lunp"
	}
	out, _ := h.Run(ctx, "ss", "-H", flag, fmt.Sprintf("sport = :%d", port))
	if m := regexp.MustCompile(`users:\(\("([^"]+)"`).FindStringSubmatch(out); m != nil {
		return m[1], true
	}
	return "unknown", true
}

func firstIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}
