package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/runtime"
	"github.com/kaiserern/shoal/internal/runtime/fake"
)

// An exec probe passes when the command exits 0 in the running container, and
// fails with the exit code and stderr otherwise (ADR 0016, pass C).
func TestExecProbeWithFakeRuntime(t *testing.T) {
	ctx := context.Background()
	rt := fake.New(nil)
	rt.AddContainer(runtime.ContainerSpec{ID: "db-1"}, runtime.ContainerStatus{State: runtime.StateRunning})
	argv, _ := json.Marshal([]string{"pg_isready", "-U", "app"})
	hc := HealthCheck{Exec: string(argv), Timeout: time.Second}
	probe := func(id string) error {
		return runCheck(ctx, rt, probeTarget{id: id, check: hc})
	}

	if err := probe("db-1"); err != nil {
		t.Fatalf("exit 0 should pass: %v", err)
	}
	if calls := rt.Calls(); len(calls) != 1 || calls[0] != "exec db-1" {
		t.Errorf("calls = %v, want one exec of db-1", calls)
	}

	rt.SetExec("db-1", func(argv []string) runtime.ExecResult {
		if argv[0] != "pg_isready" || len(argv) != 3 {
			t.Errorf("argv = %v", argv)
		}
		return runtime.ExecResult{ExitCode: 2, Stderr: "no response\n"}
	})
	if err := probe("db-1"); err == nil || !strings.Contains(err.Error(), "exited with 2: no response") {
		t.Fatalf("exit 2 error = %v", err)
	}

	if err := probe("missing"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("unknown container error = %v, want ErrNotFound", err)
	}

	rt.Exit("db-1", 1)
	if err := probe("db-1"); err == nil {
		t.Error("a stopped container must fail its exec probe")
	}
}

// The fake caps exec output like containerd does.
func TestExecOutputIsCapped(t *testing.T) {
	ctx := context.Background()
	rt := fake.New(nil)
	rt.AddContainer(runtime.ContainerSpec{ID: "c"}, runtime.ContainerStatus{State: runtime.StateRunning})
	rt.SetExec("c", func([]string) runtime.ExecResult {
		return runtime.ExecResult{Stdout: strings.Repeat("x", 2*runtime.ExecOutputLimit)}
	})
	res, err := rt.Exec(ctx, "c", []string{"cat"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != runtime.ExecOutputLimit || !res.Truncated {
		t.Errorf("stdout %d bytes, truncated %v; want %d and true", len(res.Stdout), res.Truncated, runtime.ExecOutputLimit)
	}
}

// An exec health check from the API reaches the agent as its argv.
func TestExecHealthCheckFromAPI(t *testing.T) {
	a, err := AssignmentFromAPI(&v1alpha1.Assignment{
		Replica:  "db-1",
		Workload: "db",
		Template: &v1alpha1.WorkloadSpec{
			Image: "postgres:16",
			HealthCheck: &v1alpha1.HealthCheck{
				Exec:             &v1alpha1.ExecHealthCheck{Command: []string{"pg_isready", "-U", "app"}},
				IntervalSeconds:  10,
				TimeoutSeconds:   5,
				FailureThreshold: 3,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Health == nil || a.Health.Exec != `["pg_isready","-U","app"]` || a.Health.HTTPPath != "" || a.Health.Port != 0 {
		t.Fatalf("health = %+v", a.Health)
	}
}
