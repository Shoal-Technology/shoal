package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kaiserern/shoal/internal/runtime"
	"github.com/kaiserern/shoal/internal/runtime/fake"
)

func TestParseAssignments(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    []Assignment
		wantErr string
	}{
		{
			name: "full replica",
			yaml: `
replicas:
  - id: web-1
    workload: web
    image: nginx:1.27-alpine
    command: [nginx, -g, "daemon off;"]
    env: {B: "2", A: "1"}
    ports: [{containerPort: 80, hostPort: 8080}]
    resources: {cpu: 200m, memory: 64Mi}
    restartPolicy: OnFailure
    desired: Stopped
`,
			want: []Assignment{{
				ReplicaID: "web-1",
				Workload:  "web",
				Spec: runtime.ContainerSpec{
					Image:       "nginx:1.27-alpine",
					Command:     []string{"nginx", "-g", "daemon off;"},
					Env:         []string{"A=1", "B=2"},
					Ports:       []runtime.PortMapping{{ContainerPort: 80, HostPort: 8080, Protocol: "tcp"}},
					CPUMillis:   200,
					MemoryBytes: 64 << 20,
				},
				RestartPolicy: RestartOnFailure,
				Desired:       DesiredStopped,
			}},
		},
		{
			name: "defaults",
			yaml: "replicas: [{id: a, image: busybox}]",
			want: []Assignment{{
				ReplicaID: "a", Workload: "a", Spec: runtime.ContainerSpec{Image: "busybox"},
				RestartPolicy: RestartAlways, Desired: DesiredRunning,
			}},
		},
		{name: "empty list", yaml: "replicas: []", want: []Assignment{}},
		{name: "unknown field", yaml: "replicas: [{id: a, image: busybox, replicas: 3}]", wantErr: "unknown field"},
		{name: "missing image", yaml: "replicas: [{id: a}]", wantErr: "image is required"},
		{name: "bad id", yaml: "replicas: [{id: Web_1, image: busybox}]", wantErr: "DNS label"},
		{name: "duplicate id", yaml: "replicas: [{id: a, image: x}, {id: a, image: y}]", wantErr: "duplicate id"},
		{name: "bad cpu", yaml: "replicas: [{id: a, image: x, resources: {cpu: lots}}]", wantErr: "cpu"},
		{name: "bad memory", yaml: "replicas: [{id: a, image: x, resources: {memory: 1XB}}]", wantErr: "memory"},
		{name: "bad protocol", yaml: "replicas: [{id: a, image: x, ports: [{containerPort: 1, protocol: sctp}]}]", wantErr: "protocol"},
		{name: "port out of range", yaml: "replicas: [{id: a, image: x, ports: [{containerPort: 70000}]}]", wantErr: "out of range"},
		{
			name:    "duplicate host port",
			yaml:    "replicas: [{id: a, image: x, ports: [{containerPort: 1, hostPort: 80}, {containerPort: 2, hostPort: 80}]}]",
			wantErr: "duplicate hostPort",
		},
		{name: "bad env name", yaml: "replicas: [{id: a, image: x, env: {1BAD: v}}]", wantErr: "env var"},
		{name: "bad restart policy", yaml: "replicas: [{id: a, image: x, restartPolicy: Sometimes}]", wantErr: "restartPolicy"},
		{name: "bad desired", yaml: "replicas: [{id: a, image: x, desired: Paused}]", wantErr: "desired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAssignments([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.EqualFunc(got, tt.want, assignmentEqual) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func assignmentEqual(a, b Assignment) bool {
	return a.ReplicaID == b.ReplicaID && a.Workload == b.Workload && a.RestartPolicy == b.RestartPolicy &&
		a.Desired == b.Desired && a.specHash() == b.specHash()
}

func TestStandaloneRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replicas.yaml")
	mtime := time.Now()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// Force a distinct mtime so the change is noticed regardless of
		// filesystem timestamp granularity.
		mtime = mtime.Add(time.Second)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	rt := fake.New(nil)
	s := &Standalone{
		Path:       path,
		Interval:   10 * time.Millisecond,
		Reconciler: NewReconciler(rt, discardLogger(), Config{}),
		Log:        discardLogger(),
	}
	waitFor := func(desc string, cond func([]runtime.ContainerStatus) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			cs, _ := rt.List(context.Background())
			if cond(cs) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}
	hasRunning := func(id string) func([]runtime.ContainerStatus) bool {
		return func(cs []runtime.ContainerStatus) bool {
			return slices.ContainsFunc(cs, func(c runtime.ContainerStatus) bool {
				return c.ID == id && c.State == runtime.StateRunning
			})
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Nothing happens until a valid file exists.
	time.Sleep(50 * time.Millisecond)
	if cs, _ := rt.List(context.Background()); len(cs) != 0 {
		t.Fatalf("containers created without a replicas file: %v", cs)
	}

	write("replicas: [{id: a, image: busybox}, {id: b, image: busybox}]")
	waitFor("a and b running", func(cs []runtime.ContainerStatus) bool {
		return hasRunning("a")(cs) && hasRunning("b")(cs)
	})

	// An invalid edit keeps the previous desired state.
	write("replicas: [{id: a}]")
	time.Sleep(50 * time.Millisecond)
	waitFor("a and b still running", func(cs []runtime.ContainerStatus) bool {
		return hasRunning("a")(cs) && hasRunning("b")(cs)
	})

	write("replicas: [{id: a, image: busybox}]")
	waitFor("b removed", func(cs []runtime.ContainerStatus) bool { return len(cs) == 1 && hasRunning("a")(cs) })

	// A crashed container is restarted.
	rt.Exit("a", 1)
	waitFor("a restarted", func(cs []runtime.ContainerStatus) bool {
		return hasRunning("a")(cs) && cs[0].Starts == 2
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Stopping the agent leaves containers running.
	if cs, _ := rt.List(context.Background()); !hasRunning("a")(cs) {
		t.Fatal("container should keep running after the agent stops")
	}
}
