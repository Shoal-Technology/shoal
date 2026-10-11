package controller

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/store"
)

const gi = int64(1) << 30

// nodeDisk is node with a disk capacity and the loop volume backend.
func (e *env) nodeDisk(name, phase string, diskGi int64, ago time.Duration) {
	e.t.Helper()
	st, err := objects.Encode(&v1alpha1.NodeStatus{
		Phase:         phase,
		LastHeartbeat: timestamppb.New(e.now.Add(-ago)),
		Capacity:      &v1alpha1.NodeCapacity{CpuMillis: 8000, MemoryBytes: 8192 << 20, DiskBytes: diskGi * gi},
		VolumeBackend: "loop",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Get(ctx, v1alpha1.KindNode, name); err == nil {
		if _, err := e.st.UpdateStatus(ctx, v1alpha1.KindNode, name, st); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Status: st}); err != nil {
		e.t.Fatal(err)
	}
}

// volume applies a Volume the way the API does (defaulted, validated).
func (e *env) volume(name, size string) {
	e.t.Helper()
	s := &v1alpha1.VolumeSpec{Size: size}
	resource.DefaultVolume(s)
	if err := resource.ValidateVolume(name, s); err != nil {
		e.t.Fatal(err)
	}
	spec, err := objects.Encode(s)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindVolume, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) volumeStatus(name string) *v1alpha1.VolumeStatus {
	e.t.Helper()
	rec, err := e.st.Get(ctx, v1alpha1.KindVolume, name)
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := objects.Volume(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	return v.Status
}

func rwVolume(name, path string) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) {
		s.Volumes = []*v1alpha1.VolumeMount{{Name: name, MountPath: path}}
	}
}

func roVolume(name, path string) func(*v1alpha1.WorkloadSpec) {
	return func(s *v1alpha1.WorkloadSpec) {
		s.Volumes = []*v1alpha1.VolumeMount{{Name: name, MountPath: path, ReadOnly: true}}
	}
}

// boundNode returns the node the scheduled replicas are on ("" if none).
func (e *env) boundNode() string {
	for n := range e.placement() {
		if n != "" {
			return n
		}
	}
	return ""
}

func (e *env) replicaNamed(workload string) *v1alpha1.Replica {
	e.t.Helper()
	for _, r := range e.replicas() {
		if r.Spec.GetWorkload() == workload {
			return r
		}
	}
	e.t.Fatalf("no replica of %s", workload)
	return nil
}

// The first replica that mounts a Volume binds it to a node. Later replicas of
// the same workload go to that node and nowhere else (ADR 0016, 1.4).
func TestFirstBindPinsVolumeAndLaterReplicasFollow(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("a", v1alpha1.NodeReady, 100, 0)
	e.nodeDisk("b", v1alpha1.NodeReady, 100, 0)
	e.volume("shared", "10Gi")
	e.workload("ro", 2, roVolume("shared", "/assets"))
	e.reconcile()

	p := e.placement()
	if len(p) != 1 || p[""] != 0 {
		t.Fatalf("placement = %v, want both replicas on one node", p)
	}
	node := e.boundNode()
	st := e.volumeStatus("shared")
	if st.GetNode() != node || st.GetState() != volumeBound || st.GetAttachedWorkload() != "ro" {
		t.Errorf("volume status = %+v, want bound to %s by ro", st, node)
	}
	if st.GetQuota() != "size" {
		t.Errorf("quota = %q, want size for a loop node", st.GetQuota())
	}
}

// A replica with a read-write volume whose node is lost is not replaced on
// another node: the volume's data is on that node. The volume is NodeLost,
// and it returns to Bound when the node comes back (ADR 0016, 1.5).
func TestEvictionSkipsVolumeReplicaAndMarksVolumeNodeLost(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("a", v1alpha1.NodeReady, 100, 0)
	e.nodeDisk("b", v1alpha1.NodeReady, 100, 0)
	e.volume("data", "10Gi")
	e.workload("db", 1, rwVolume("data", "/var/lib/db"))
	e.reconcile()
	home := e.boundNode()
	if home == "" {
		t.Fatal("database replica not scheduled")
	}
	other := "a"
	if home == "a" {
		other = "b"
	}
	replicaName := e.replicaNamed("db").GetMetadata().GetName()

	// Long past the eviction window.
	e.nodeDisk(home, v1alpha1.NodeNotReady, 100, 10*time.Minute)
	e.nodeDisk(other, v1alpha1.NodeReady, 100, 0)
	e.reconcile()

	if p := e.placement(); p[home] != 1 || p[other] != 0 {
		t.Fatalf("placement after loss = %v, want the replica to stay on %s", p, home)
	}
	rp := e.replicaNamed("db")
	if rp.GetMetadata().GetName() != replicaName {
		t.Errorf("replica was replaced: %s -> %s", replicaName, rp.GetMetadata().GetName())
	}
	if !strings.Contains(rp.GetStatus().GetMessage(), "stays with its volume") {
		t.Errorf("replica message = %q", rp.GetStatus().GetMessage())
	}
	if st := e.volumeStatus("data"); st.GetState() != volumeNodeLost || st.GetNode() != home {
		t.Errorf("volume status = %+v, want NodeLost on %s", st, home)
	}

	// The node comes back: the data is intact and the volume is Bound again.
	e.nodeDisk(home, v1alpha1.NodeReady, 100, 0)
	e.reconcile()
	if st := e.volumeStatus("data"); st.GetState() != volumeBound {
		t.Errorf("after recovery volume status = %+v, want Bound", st)
	}
	if p := e.placement(); p[home] != 1 {
		t.Errorf("placement after recovery = %v", p)
	}
}

// A replica that is recreated while its volume's node is not Ready waits, with
// the reason, and does not move to another node.
func TestRecreatedReplicaWaitsForVolumeNode(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("a", v1alpha1.NodeReady, 100, 0)
	e.nodeDisk("b", v1alpha1.NodeReady, 100, 0)
	e.volume("data", "10Gi")
	e.workload("db", 1, rwVolume("data", "/d"))
	e.reconcile()
	home := e.boundNode()
	other := "a"
	if home == "a" {
		other = "b"
	}
	e.nodeDisk(home, v1alpha1.NodeNotReady, 100, 30*time.Second) // not yet evicted
	rp := e.replicaNamed("db")
	if err := e.st.Delete(ctx, v1alpha1.KindReplica, rp.GetMetadata().GetName(), 0); err != nil {
		t.Fatal(err)
	}
	e.reconcile()

	fresh := e.replicaNamed("db")
	if fresh.Spec.GetNode() != "" {
		t.Fatalf("recreated replica bound to %q, want unscheduled while %s is not ready", fresh.Spec.GetNode(), home)
	}
	want := "volume data is on node " + home + ", which is not Ready"
	if fresh.GetStatus().GetMessage() != want {
		t.Errorf("message = %q, want %q", fresh.GetStatus().GetMessage(), want)
	}
	if p := e.placement(); p[other] != 0 {
		t.Errorf("replica moved to %s: %v", other, p)
	}
}

// Volume sizes count against node disk: a volume bound on a node uses its
// space, so a second large volume does not fit there.
func TestVolumeSizesCountAgainstNodeDisk(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("small", v1alpha1.NodeReady, 5, 0)
	e.nodeDisk("big", v1alpha1.NodeReady, 20, 0)

	e.volume("v1", "15Gi")
	e.workload("w1", 1, rwVolume("v1", "/d"))
	e.reconcile()
	if e.boundNode() != "big" {
		t.Fatalf("w1 placed %v, want big (small has 5Gi)", e.placement())
	}

	e.volume("v2", "10Gi")
	e.workload("w2", 1, rwVolume("v2", "/d"))
	e.reconcile()
	rp := e.replicaNamed("w2")
	if rp.Spec.GetNode() != "" {
		t.Fatalf("w2 placed on %q although big has 5Gi free", rp.Spec.GetNode())
	}
	if !strings.Contains(rp.GetStatus().GetMessage(), "disk 10240Mi") {
		t.Errorf("message = %q, want the disk need", rp.GetStatus().GetMessage())
	}
	if st := e.volumeStatus("v2"); st.GetNode() != "" || st.GetState() != volumePending {
		t.Errorf("v2 status = %+v, want unbound", st)
	}
}

// A node that reports no disk capacity takes no new volume: its free space is
// unknown.
func TestNodeWithoutDiskCapacityTakesNoNewVolume(t *testing.T) {
	e := newEnv(t)
	e.node("nodisk", v1alpha1.NodeReady, 8000, 8192, 0)
	e.volume("v", "1Gi")
	e.workload("w", 1, rwVolume("v", "/d"))
	e.reconcile()
	if rp := e.replicaNamed("w"); rp.Spec.GetNode() != "" {
		t.Errorf("placed on a node with unknown disk: %q", rp.Spec.GetNode())
	}
}

func TestReplicaWaitsForMissingVolume(t *testing.T) {
	e := newEnv(t)
	e.nodeDisk("a", v1alpha1.NodeReady, 100, 0)
	e.workload("w", 1, rwVolume("absent", "/d"))
	e.reconcile()
	rp := e.replicaNamed("w")
	if rp.Spec.GetNode() != "" || !strings.Contains(rp.GetStatus().GetMessage(), "volume absent not found") {
		t.Errorf("replica node=%q message=%q", rp.Spec.GetNode(), rp.GetStatus().GetMessage())
	}
}
