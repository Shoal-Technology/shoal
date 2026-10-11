package server

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/runtime"
)

func applyYAML(t *testing.T, admin v1alpha1.ControlClient, y string) {
	t.Helper()
	if _, err := admin.Apply(context.Background(), &v1alpha1.ApplyRequest{Objects: decode(t, y)}); err != nil {
		t.Fatal(err)
	}
}

func webWith(image string, replicas int) string {
	return strings.NewReplacer("nginx:1.27", image, "replicas: 3", "replicas: "+strconv.Itoa(replicas)).Replace(webYAML)
}

func workloadStatus(t *testing.T, admin v1alpha1.ControlClient, name string) *v1alpha1.Workload {
	t.Helper()
	obj, err := admin.Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return obj.GetWorkload()
}

// runningImages counts running containers per image on the given agents.
func runningImages(t *testing.T, agents ...*testAgent) map[string]int {
	t.Helper()
	m := map[string]int{}
	for _, a := range agents {
		cs, err := a.rt.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.State == runtime.StateRunning {
				m[c.Image]++
			}
		}
	}
	return m
}

// TestRollingUpdateAndRollback is Phase 4's end-to-end check with two
// agents: an update rolls out without more than maxUnavailable replicas
// down, and an update whose containers crash is rolled back automatically.
func TestRollingUpdateAndRollback(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, fastControlPlane)
	admin := ts.admin(t)
	a1 := ts.startAgent(t, admin, "node-1")
	a2 := ts.startAgent(t, admin, "node-2")
	eventually(t, "nodes ready", func() bool {
		return getNode(t, admin, "node-1").GetStatus().GetPhase() == NodeReady &&
			getNode(t, admin, "node-2").GetStatus().GetPhase() == NodeReady
	})

	applyYAML(t, admin, webWith("nginx:1.27", 4))
	eventually(t, "first rollout complete", func() bool {
		w := workloadStatus(t, admin, "web")
		return w.GetStatus().GetReadyReplicas() == 4 && w.GetStatus().GetRollout().GetPhase() == "Complete"
	})

	// Watch the ready count during the rolling update.
	var (
		minReady atomic.Int32
		stop     = make(chan struct{})
		wg       sync.WaitGroup
	)
	minReady.Store(4)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			obj, err := admin.Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
			if err != nil {
				continue
			}
			if r := obj.GetWorkload().GetStatus().GetReadyReplicas(); r < minReady.Load() {
				minReady.Store(r)
			}
		}
	}()

	applyYAML(t, admin, webWith("nginx:1.28", 4))
	eventually(t, "rolling update complete", func() bool {
		ro := workloadStatus(t, admin, "web").GetStatus().GetRollout()
		return ro.GetPhase() == "Complete" && ro.GetStableGeneration() == 2 && runningImages(t, a1, a2)["nginx:1.28"] == 4
	})
	close(stop)
	wg.Wait()
	if m := minReady.Load(); m < 3 {
		t.Fatalf("ready replicas dropped to %d during the update; maxUnavailable is 1", m)
	}

	// A broken release: its containers crash on start.
	a1.rt.CrashImage("nginx:broken")
	a2.rt.CrashImage("nginx:broken")
	applyYAML(t, admin, webWith("nginx:broken", 4))
	eventually(t, "automatic rollback", func() bool {
		w := workloadStatus(t, admin, "web")
		return w.GetStatus().GetRollout().GetPhase() == "RolledBack" && w.GetStatus().GetReadyReplicas() == 4 &&
			runningImages(t, a1, a2)["nginx:1.28"] == 4
	})
	w := workloadStatus(t, admin, "web")
	if w.GetSpec().GetImage() != "nginx:broken" || !strings.Contains(w.GetStatus().GetRollout().GetMessage(), "rolled back to generation 2") {
		t.Fatalf("after rollback: spec image %q, rollout %v", w.GetSpec().GetImage(), w.GetStatus().GetRollout())
	}
}

func collectLogs(t *testing.T, admin v1alpha1.ControlClient, req *v1alpha1.LogsRequest) ([]*v1alpha1.LogLine, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := admin.Logs(ctx, req)
	if err != nil {
		return nil, err
	}
	var lines []*v1alpha1.LogLine
	for {
		l, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
		lines = append(lines, l)
	}
}

func TestLogs(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, fastControlPlane)
	admin := ts.admin(t)
	a1 := ts.startAgent(t, admin, "node-1")
	eventually(t, "node ready", func() bool { return getNode(t, admin, "node-1").GetStatus().GetPhase() == NodeReady })
	applyYAML(t, admin, webWith("nginx:1.27", 2))
	eventually(t, "2 running", func() bool { return len(a1.running(t)) == 2 })

	ids := a1.running(t)
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	a1.rt.SetLogs(ids[0], []runtime.LogEntry{
		{Time: t0, Stream: "stdout", Line: "first"},
		{Time: t0.Add(2 * time.Second), Stream: "stdout", Line: "third"},
	})
	a1.rt.SetLogs(ids[1], []runtime.LogEntry{{Time: t0.Add(time.Second), Stream: "stderr", Line: "second"}})

	lines, err := collectLogs(t, admin, &v1alpha1.LogsRequest{Workload: "web", Tail: -1})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.GetReplica()+":"+l.GetStream()+":"+l.GetLine())
	}
	want := []string{ids[0] + ":stdout:first", ids[1] + ":stderr:second", ids[0] + ":stdout:third"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("merged logs = %q\nwant %q", got, want)
	}

	lines, err = collectLogs(t, admin, &v1alpha1.LogsRequest{Workload: "web", Replica: ids[0], Tail: 1})
	if err != nil || len(lines) != 1 || lines[0].GetLine() != "third" {
		t.Fatalf("one replica, tail 1: %v, %v", lines, err)
	}

	_, err = collectLogs(t, admin, &v1alpha1.LogsRequest{Workload: "nope"})
	wantCode(t, err, codes.NotFound, "no replicas")

	// A replica whose node is gone yields a note instead of hanging.
	a1.stop()
	lines, err = collectLogs(t, admin, &v1alpha1.LogsRequest{Workload: "web", Replica: ids[0]})
	if err != nil || len(lines) != 1 || lines[0].GetStream() != noteStream || !strings.Contains(lines[0].GetLine(), "not connected") {
		t.Fatalf("logs from a disconnected node: %v, %v", lines, err)
	}
}
