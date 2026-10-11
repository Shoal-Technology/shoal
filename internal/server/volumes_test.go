package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// newVolumeServer returns a control service over an in-memory store: enough
// of a Server for the apply, get and delete paths.
func newVolumeServer(t *testing.T) (*Server, *controlService) {
	t.Helper()
	s := &Server{store: memory.New(time.Now), log: discard()}
	return s, &controlService{s: s}
}

func volumeObj(name, size string) *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Volume{Volume: &v1alpha1.Volume{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.VolumeSpec{Size: size},
	}}}
}

// workloadObj is a single-replica workload that mounts each named volume at
// /data/<name>.
func workloadObj(name string, volumes ...string) *v1alpha1.Object {
	w := &v1alpha1.Workload{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.WorkloadSpec{Image: "postgres:16", Replicas: proto.Int32(1)},
	}
	for _, v := range volumes {
		w.Spec.Volumes = append(w.Spec.Volumes, &v1alpha1.VolumeMount{Name: v, MountPath: "/data/" + v})
	}
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Workload{Workload: w}}
}

func applyObjs(t *testing.T, c *controlService, objs ...*v1alpha1.Object) error {
	t.Helper()
	_, err := c.Apply(context.Background(), &v1alpha1.ApplyRequest{Objects: objs})
	return err
}

// Many workloads race to claim one Volume: exactly one wins, and the others
// are refused, never half-applied (ADR 0016, 1.1).
func TestConcurrentApplyClaimsVolumeExactlyOnce(t *testing.T) {
	s, c := newVolumeServer(t)
	if err := applyObjs(t, c, volumeObj("data", "10Gi")); err != nil {
		t.Fatal(err)
	}
	const racers = 16
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- applyObjs(t, c, workloadObj(fmt.Sprintf("w%d", i), "data"))
		}()
	}
	wg.Wait()
	close(errs)
	var won, refused int
	for err := range errs {
		switch {
		case err == nil:
			won++
		case status.Code(err) == codes.InvalidArgument:
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 || refused != racers-1 {
		t.Fatalf("won %d, refused %d; want 1 and %d", won, refused, racers-1)
	}
	recs, err := s.store.List(context.Background(), v1alpha1.KindWorkload)
	if err != nil || len(recs) != 1 {
		t.Errorf("workloads stored = %d (%v), want 1", len(recs), err)
	}
}

// A refused apply leaves no claim behind: the Volume that was claimed before
// the conflict is free again.
func TestRefusedApplyReleasesClaimsItTook(t *testing.T) {
	_, c := newVolumeServer(t)
	for _, v := range []string{"a", "b"} {
		if err := applyObjs(t, c, volumeObj(v, "1Gi")); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyObjs(t, c, workloadObj("owner", "a")); err != nil {
		t.Fatal(err)
	}
	// Claims b first, then finds a taken: the whole apply is refused.
	err := applyObjs(t, c, workloadObj("late", "b", "a"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
	if err := applyObjs(t, c, workloadObj("other", "b")); err != nil {
		t.Fatalf("b was left claimed by the refused apply: %v", err)
	}
}

// A workload that stops naming a Volume gives up its claim.
func TestDroppedVolumeIsReleased(t *testing.T) {
	_, c := newVolumeServer(t)
	for _, v := range []string{"data", "spare"} {
		if err := applyObjs(t, c, volumeObj(v, "1Gi")); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyObjs(t, c, workloadObj("db", "data")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("db", "spare")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("next", "data")); err != nil {
		t.Fatalf("data still claimed after db dropped it: %v", err)
	}
	if err := applyObjs(t, c, workloadObj("again", "spare")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("spare is held by db, got %v", err)
	}
}

// Deleting a Workload frees its Volumes.
func TestDeletingWorkloadFreesVolume(t *testing.T) {
	_, c := newVolumeServer(t)
	ctx := context.Background()
	if err := applyObjs(t, c, volumeObj("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("db", "data")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "db"}); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("next", "data")); err != nil {
		t.Fatalf("volume still claimed after the workload was deleted: %v", err)
	}
}

// A Volume named by a Workload cannot be deleted, and the refusal leaves the
// Volume and its claim as they were.
func TestDeleteVolumeRefusedWhileReferenced(t *testing.T) {
	s, c := newVolumeServer(t)
	ctx := context.Background()
	if err := applyObjs(t, c, volumeObj("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("db", "data")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindVolume, Name: "data"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete of a used volume: %v", err)
	}
	if _, err := s.store.Get(ctx, v1alpha1.KindVolume, "data"); err != nil {
		t.Errorf("volume gone after refused delete: %v", err)
	}
	if owner, held, _ := s.claimOwner(ctx, "data"); !held || owner != "db" {
		t.Errorf("claim after refused delete = %q (held %v), want db", owner, held)
	}
	// Once the workload is gone the Volume can be deleted, and its marker
	// claim does not linger.
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "db"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindVolume, Name: "data"}); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := s.claimOwner(ctx, "data"); held {
		t.Error("claim record left after the volume was deleted")
	}
}

func TestApplyRefusesMissingVolume(t *testing.T) {
	_, c := newVolumeServer(t)
	err := applyObjs(t, c, workloadObj("db", "nope"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument for a missing volume", err)
	}
}

// A Volume and the Workload that uses it can come in one batch.
func TestApplyBatchWithVolumeAndWorkload(t *testing.T) {
	_, c := newVolumeServer(t)
	if err := applyObjs(t, c, volumeObj("data", "1Gi"), workloadObj("db", "data")); err != nil {
		t.Fatalf("batch refused: %v", err)
	}
}

// A bound Volume cannot be resized: online resize is out of scope (ADR 0016).
func TestBoundVolumeCannotChangeSize(t *testing.T) {
	s, c := newVolumeServer(t)
	ctx := context.Background()
	if err := applyObjs(t, c, volumeObj("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.store.Get(ctx, v1alpha1.KindVolume, "data")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := objects.Encode(&v1alpha1.VolumeStatus{Node: "n1", State: "Bound"})
	if _, err := s.store.UpdateStatus(ctx, v1alpha1.KindVolume, rec.Name, st); err != nil {
		t.Fatal(err)
	}
	err = applyObjs(t, c, volumeObj("data", "2Gi"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("resize of a bound volume: %v", err)
	}
	// Re-applying the same size is fine.
	if err := applyObjs(t, c, volumeObj("data", "1Gi")); err != nil {
		t.Fatalf("unchanged apply refused: %v", err)
	}
}

var _ store.Store = (*memory.Store)(nil)
