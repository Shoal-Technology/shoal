//go:build linux && integration

// Integration tests against a real containerd. They need root, containerd
// 2.x, CNI plugins and network access to pull busybox. Run with `make
// integration`, which builds the shoal binary used as logging driver.
package containerd_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"golang.org/x/sys/unix"

	"example.com/shoal/internal/agent"
	"example.com/shoal/internal/runtime"
	"example.com/shoal/internal/runtime/containerd"
)

var shoalBin = flag.String("shoal-bin", "", "path to the shoal binary (log driver)")

const (
	testImage = "docker.io/library/busybox:1.36"
	testCIDR  = "10.251.0.0/24"
)

func newRuntime(t *testing.T) containerd.Client {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests need root")
	}
	if *shoalBin == "" {
		t.Skip("-shoal-bin not set")
	}
	ctx := context.Background()
	rt, err := containerd.New(ctx, containerd.Options{
		Namespace:     "shoal-integration",
		StateDir:      t.TempDir(),
		PodCIDR:       testCIDR,
		BridgeName:    "shoalit0",
		NetworkName:   "shoal-integration",
		LogDriverPath: *shoalBin,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		cs, err := rt.List(ctx)
		if err != nil {
			t.Errorf("cleanup list: %v", err)
		}
		for _, c := range cs {
			if err := rt.Remove(ctx, c.ID); err != nil {
				t.Errorf("cleanup %s: %v", c.ID, err)
			}
		}
	}
	cleanup() // leftovers from an aborted run
	t.Cleanup(func() { cleanup(); rt.Close() })
	if err := rt.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	return rt
}

func eventually(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, desc)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func logLines(t *testing.T, rt runtime.Runtime, id string) map[string]string {
	t.Helper()
	lines := map[string]string{}
	err := rt.Logs(context.Background(), id, runtime.LogOptions{Tail: -1}, func(e runtime.LogEntry) error {
		lines[e.Line] = e.Stream
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func status(t *testing.T, rt runtime.Runtime, id string) runtime.ContainerStatus {
	t.Helper()
	st, err := rt.Status(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLifecycle(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-lifecycle"
	err := rt.Create(ctx, runtime.ContainerSpec{
		ID:    id,
		Image: testImage,
		Env:   []string{"GREETING=hello"},
		Command: []string{"sh", "-c",
			`echo "$GREETING-out"; echo "$GREETING-err" >&2; trap 'echo bye; exit 0' TERM; while true; do sleep 0.2; done`},
		Labels: map[string]string{"test": "lifecycle"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Create(ctx, runtime.ContainerSpec{ID: id, Image: testImage}); !errors.Is(err, runtime.ErrAlreadyExists) {
		t.Fatalf("second Create: err = %v, want ErrAlreadyExists", err)
	}
	if st := status(t, rt, id); st.State != runtime.StateCreated || st.Labels["test"] != "lifecycle" {
		t.Fatalf("after create: %+v", st)
	}

	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	st := status(t, rt, id)
	if st.State != runtime.StateRunning || st.Starts != 1 {
		t.Fatalf("after start: %+v", st)
	}
	if ip, err := netip.ParseAddr(st.IP); err != nil || !netip.MustParsePrefix(testCIDR).Contains(ip) {
		t.Fatalf("IP %q not in %s", st.IP, testCIDR)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st := status(t, rt, id); st.Starts != 1 {
		t.Fatalf("Start on running container must be a no-op, starts = %d", st.Starts)
	}

	eventually(t, 5*time.Second, "stdout and stderr logs", func() bool {
		l := logLines(t, rt, id)
		return l["hello-out"] == "stdout" && l["hello-err"] == "stderr"
	})

	if err := rt.Stop(ctx, id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	st = status(t, rt, id)
	if st.State != runtime.StateExited || st.ExitCode != 0 || !st.StopRequested {
		t.Fatalf("after graceful stop: %+v", st)
	}
	if _, ok := logLines(t, rt, id)["bye"]; !ok {
		t.Fatal("SIGTERM handler output missing from logs")
	}
	if err := rt.Stop(ctx, id, time.Second); err != nil {
		t.Fatalf("Stop on exited container must be a no-op: %v", err)
	}

	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st := status(t, rt, id); st.State != runtime.StateRunning || st.Starts != 2 || st.StopRequested {
		t.Fatalf("after restart: %+v", st)
	}
	// Logs persist across restarts.
	eventually(t, 5*time.Second, "logs from both runs", func() bool {
		n := 0
		_ = rt.Logs(ctx, id, runtime.LogOptions{Tail: -1}, func(e runtime.LogEntry) error {
			if e.Line == "hello-out" {
				n++
			}
			return nil
		})
		return n == 2
	})

	if err := rt.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Status(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("after remove: err = %v, want ErrNotFound", err)
	}
	if err := rt.Remove(ctx, id); err != nil {
		t.Fatalf("Remove must be idempotent: %v", err)
	}
}

// TestFollowLogsEndsOnExit checks that a follow returns once the container
// exits, including the output written right before the exit.
func TestFollowLogsEndsOnExit(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-follow"
	cmd := []string{"sh", "-c", "echo first; sleep 1; echo last"}
	if err := rt.Create(ctx, runtime.ContainerSpec{ID: id, Image: testImage, Command: cmd}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var lines []string
	err := rt.Logs(fctx, id, runtime.LogOptions{Follow: true, Tail: -1}, func(e runtime.LogEntry) error {
		lines = append(lines, e.Line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fctx.Err() != nil {
		t.Fatal("follow did not end when the container exited")
	}
	if strings.Join(lines, ",") != "first,last" {
		t.Fatalf("lines = %q, want [first last]", lines)
	}
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-sigkill"
	cmd := []string{"sh", "-c", `trap '' TERM; while true; do sleep 0.2; done`}
	if err := rt.Create(ctx, runtime.ContainerSpec{ID: id, Image: testImage, Command: cmd}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rt.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < time.Second || d > 10*time.Second {
		t.Fatalf("stop took %s, want about 1s", d)
	}
	if st := status(t, rt, id); st.State != runtime.StateExited || st.ExitCode != 137 {
		t.Fatalf("after SIGKILL: %+v, want exit code 137", st)
	}
}

func TestExitCodeAndResourceLimits(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-limits"
	err := rt.Create(ctx, runtime.ContainerSpec{
		ID:          id,
		Image:       testImage,
		Command:     []string{"sh", "-c", "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/cpu.max; exit 3"},
		CPUMillis:   200,
		MemoryBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "container exit", func() bool {
		return status(t, rt, id).State == runtime.StateExited
	})
	if st := status(t, rt, id); st.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", st.ExitCode)
	}
	eventually(t, 5*time.Second, "cgroup limits in logs", func() bool {
		l := logLines(t, rt, id)
		_, mem := l[fmt.Sprint(64<<20)]
		_, cpu := l["20000 100000"]
		return mem && cpu
	})
}

func TestNetworkingAndPublishedPort(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-http"
	const hostPort = 18080
	err := rt.Create(ctx, runtime.ContainerSpec{
		ID:      id,
		Image:   testImage,
		Command: []string{"sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"},
		Ports:   []runtime.PortMapping{{ContainerPort: 8080, HostPort: hostPort}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	ip := status(t, rt, id).IP

	get := func(url string) (string, error) {
		c := http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b)), err
	}
	check := func(desc, url string) {
		t.Helper()
		eventually(t, 10*time.Second, desc+" ("+url+")", func() bool {
			body, err := get(url)
			return err == nil && body == "ok"
		})
	}
	check("container IP", fmt.Sprintf("http://%s:8080/index.html", ip))
	if hostIP := firstHostIPv4(t); hostIP != "" {
		check("published host port", fmt.Sprintf("http://%s:%d/index.html", hostIP, hostPort))
	}

	// Removing the container releases the port mapping.
	if err := rt.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if hostIP := firstHostIPv4(t); hostIP != "" {
		if _, err := get(fmt.Sprintf("http://%s:%d/", hostIP, hostPort)); err == nil {
			t.Fatal("port mapping still active after Remove")
		}
	}
}

// natRules counts iptables NAT rules that CNI created for container id.
func natRules(t *testing.T, id string) int {
	t.Helper()
	out, err := exec.Command("iptables-save", "-t", "nat").Output()
	if err != nil {
		t.Fatalf("iptables-save: %v", err)
	}
	return strings.Count(string(out), fmt.Sprintf(`id: \"%s\"`, id))
}

// TestNetworkSurvivesRestartsAndIsCleanedUp checks the pinned netns: the IP
// is kept across restarts, restarts don't pile up iptables rules, and
// Remove leaves none behind.
func TestNetworkSurvivesRestartsAndIsCleanedUp(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-netns"
	if err := rt.Create(ctx, runtime.ContainerSpec{ID: id, Image: testImage, Command: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	ip := status(t, rt, id).IP
	rules := natRules(t, id)
	if ip == "" || rules == 0 {
		t.Fatalf("after start: ip=%q, %d NAT rules; want an IP and masquerade rules", ip, rules)
	}
	for i := range 3 {
		if err := rt.Stop(ctx, id, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := rt.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got := status(t, rt, id).IP; got != ip {
			t.Fatalf("restart %d: IP changed from %s to %s", i+1, ip, got)
		}
	}
	if got := natRules(t, id); got != rules {
		t.Fatalf("NAT rules after 3 restarts = %d, want %d (restarts must not leak rules)", got, rules)
	}
	if err := rt.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := natRules(t, id); got != 0 {
		t.Fatalf("%d NAT rules left after Remove", got)
	}
}

// TestNetworkRecoversLostNetNS simulates a host reboot, which loses the
// pinned netns bind mount: the next start must set up a new one.
func TestNetworkRecoversLostNetNS(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	const id = "it-lostns"
	if err := rt.Create(ctx, runtime.ContainerSpec{ID: id, Image: testImage, Command: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}

	raw, err := client.New("/run/containerd/containerd.sock", client.WithDefaultNamespace("shoal-integration"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	c, err := raw.LoadContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := c.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := labels["shoal.runtime.netns"]
	if path == "" {
		t.Fatal("no pinned netns recorded")
	}
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
		t.Fatalf("unmount %s: %v", path, err)
	}

	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("start after losing the netns: %v", err)
	}
	st := status(t, rt, id)
	if st.State != runtime.StateRunning || st.IP == "" {
		t.Fatalf("after recovery: %+v", st)
	}
	labels, _ = c.Labels(ctx)
	if labels["shoal.runtime.netns"] == path {
		t.Fatal("netns path not replaced")
	}
	// The rules of the lost netns (iptables survived, unlike a reboot) must
	// not block removal or stay behind.
	if err := rt.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := natRules(t, id); got != 0 {
		t.Fatalf("%d NAT rules left after Remove", got)
	}
}

func firstHostIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() &&
			!netip.MustParsePrefix(testCIDR).Contains(netip.MustParseAddr(n.IP.To4().String())) {
			return n.IP.String()
		}
	}
	return ""
}

// TestAgentRecoversCrashedContainer runs the real reconciler against
// containerd: a crashing container is restarted, and removing the
// assignment removes the container.
func TestAgentRecoversCrashedContainer(t *testing.T) {
	rt := newRuntime(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := agent.NewReconciler(rt, log, agent.Config{StopTimeout: 2 * time.Second, BackoffInitial: 200 * time.Millisecond})
	defer r.Close()
	desired := []agent.Assignment{{
		ReplicaID:     "it-agent",
		Workload:      "it",
		Spec:          runtime.ContainerSpec{Image: testImage, Command: []string{"sh", "-c", "echo up; sleep 1; exit 1"}},
		RestartPolicy: agent.RestartAlways,
		Desired:       agent.DesiredRunning,
	}}
	eventually(t, 30*time.Second, "two restarts", func() bool {
		st, err := r.Reconcile(ctx, desired)
		if err != nil {
			t.Logf("reconcile: %v", err)
			return false
		}
		return st[0].Restarts >= 2
	})
	if _, err := r.Reconcile(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Status(ctx, "it-agent"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("unassigned container not removed: %v", err)
	}
}
