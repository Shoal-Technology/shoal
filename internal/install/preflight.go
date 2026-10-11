package install

import (
	"fmt"
	"strconv"
	"strings"
)

// Check is one read-only preflight result.
type Check struct {
	Name   string
	Level  string // "ok", "WARN" or "FAIL"
	Detail string
}

// Fail reports whether the check stops init.
func (c Check) Fail() bool { return c.Level == "FAIL" }

// Line formats the check for the output.
func (c Check) Line() string { return fmt.Sprintf("%-6s preflight %s: %s", c.Level, c.Name, c.Detail) }

// Preflight runs every read-only check against the facts (ADR 0017, 1.2 step 1).
// Any FAIL means init changes nothing and exits 2.
func Preflight(f Facts, join bool) []Check {
	var out []Check
	add := func(name string, ok bool, pass, fail string) {
		if ok {
			out = append(out, Check{name, "ok", pass})
		} else {
			out = append(out, Check{name, "FAIL", fail})
		}
	}
	warn := func(name string, ok bool, pass, msg string) {
		if ok {
			out = append(out, Check{name, "ok", pass})
		} else {
			out = append(out, Check{name, "WARN", msg})
		}
	}

	add("root", f.Root, "running as root", "run with sudo")
	add("os", f.OS == "linux", "linux", "shoal init needs linux, found "+f.OS)
	add("arch", f.Arch == "amd64" || f.Arch == "arm64", f.Arch, "unsupported architecture "+f.Arch)
	add("systemd", f.SystemdRunning, "systemd is running", "systemd is not running (/run/systemd/system is missing)")
	add("cgroup", f.Cgroup2, "cgroup v2", "cgroup v2 is not mounted at /sys/fs/cgroup")
	add("overlayfs", f.OverlayFS, "overlayfs available", "overlayfs is not in /proc/filesystems")
	add("br_netfilter", f.BrNetfilter || f.BrNetfilterLoaded, "br_netfilter can be loaded",
		"the br_netfilter kernel module cannot be loaded")
	add("tools", f.Tools["iptables"] && f.Tools["ip"], "iptables and ip found",
		"install iptables and iproute2 first")

	kmaj, kmin, kok := kernelVersion(f.Kernel)
	warn("kernel", kok && (kmaj > 5 || (kmaj == 5 && kmin >= 10)), f.Kernel,
		fmt.Sprintf("kernel %q is older than 5.10", f.Kernel))
	warn("loop-control", f.LoopControl, "/dev/loop-control present",
		"/dev/loop-control is missing; volumes need the dir backend")

	ports := []int{80, 443, 7443, 7444}
	if join {
		ports = append(ports, 51820)
	}
	for _, p := range ports {
		owner, busy := f.PortOwners[p]
		if !busy || owner == "shoal" {
			out = append(out, Check{fmt.Sprintf("port-%d", p), "ok", "free or held by shoal"})
			continue
		}
		out = append(out, Check{fmt.Sprintf("port-%d", p), "FAIL",
			fmt.Sprintf("port %d is held by %s; stop it or move it", p, owner)})
	}

	warn("disk", f.DiskFreeMiB >= 15*1024, fmt.Sprintf("%d MiB free under /var/lib", f.DiskFreeMiB),
		fmt.Sprintf("%d MiB free under /var/lib; 15 GiB or more is recommended", f.DiskFreeMiB))
	add("disk-min", f.DiskFreeMiB >= 8*1024, fmt.Sprintf("%d MiB free under /var/lib", f.DiskFreeMiB),
		fmt.Sprintf("need 8 GiB free under /var/lib, have %d MiB", f.DiskFreeMiB))

	add("memory", f.MemTotalMiB >= 900, fmt.Sprintf("%d MiB of memory", f.MemTotalMiB),
		fmt.Sprintf("need at least 900 MiB of memory, have %d MiB", f.MemTotalMiB))
	warn("swap", f.MemTotalMiB >= 2048 || f.SwapMiB > 0, fmt.Sprintf("%d MiB of swap", f.SwapMiB),
		"under 2 GiB of memory and no swap; --swap 1G may help")

	if f.ContainerdVersion != "" {
		add("containerd", strings.HasPrefix(f.ContainerdVersion, "2."),
			"containerd "+f.ContainerdVersion+" will be reused",
			"containerd "+f.ContainerdVersion+" is not 2.x; remove it or upgrade it first")
		add("runc", f.Tools["runc"] || !strings.HasPrefix(f.ContainerdVersion, "2."),
			"runc found", "runc is required next to the containerd that will be reused")
	}
	return out
}

// kernelVersion parses "6.8.0-45-generic" into major and minor.
func kernelVersion(release string) (int, int, bool) {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	minStr := parts[1]
	if i := strings.IndexFunc(minStr, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minStr = minStr[:i]
	}
	minor, err2 := strconv.Atoi(minStr)
	return maj, minor, err1 == nil && err2 == nil
}
