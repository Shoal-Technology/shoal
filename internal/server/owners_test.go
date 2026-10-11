package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/store"
)

func dbObj(name string) *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Database{Database: &v1alpha1.Database{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.DatabaseSpec{Engine: "postgres", Version: "16.15", Size: "5Gi"},
	}}}
}

func credsObj(name string) *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: &v1alpha1.Secret{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.SecretSpec{Data: map[string]string{"password": "x"}},
	}}}
}

// claimOwnedBy records that db owns kind/name, as the controller does.
func claimOwnedBy(t *testing.T, s *Server, kind, name, db string) {
	t.Helper()
	if _, err := s.store.Create(context.Background(), store.Record{Kind: v1alpha1.KindOwnerClaim, Name: kind + "/" + name, Spec: []byte(db)}); err != nil {
		t.Fatal(err)
	}
}

// A user cannot edit or delete an object a Database owns (ADR 0016, pass C).
// Changing the Database is the way.
func TestOwnedObjectsAreRefusedToUsers(t *testing.T) {
	s, c := newVolumeServer(t)
	ctx := context.Background()

	if err := applyObjs(t, c, workloadObj("app-db")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, credsObj("app-db-credentials")); err != nil {
		t.Fatal(err)
	}
	claimOwnedBy(t, s, v1alpha1.KindWorkload, "app-db", "app-db")
	claimOwnedBy(t, s, v1alpha1.KindSecret, "app-db-credentials", "app-db")

	// Apply: refused with the reason, and nothing is written.
	err := applyObjs(t, c, workloadObj("app-db"))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "managed by database") {
		t.Errorf("apply of an owned workload = %v, want InvalidArgument: managed by database", err)
	}
	if err := applyObjs(t, c, credsObj("app-db-credentials")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("apply of an owned secret = %v, want InvalidArgument", err)
	}

	// Delete: refused.
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "app-db"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete of an owned workload = %v, want FailedPrecondition", err)
	}
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindSecret, Name: "app-db-credentials"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete of an owned secret = %v, want FailedPrecondition", err)
	}

	// Restart: refused.
	if _, err := c.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: "app-db"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("restart of an owned workload = %v, want FailedPrecondition", err)
	}

	// The objects survived every refused change.
	if _, err := s.store.Get(ctx, v1alpha1.KindWorkload, "app-db"); err != nil {
		t.Errorf("owned workload gone: %v", err)
	}
	if _, err := s.store.Get(ctx, v1alpha1.KindSecret, "app-db-credentials"); err != nil {
		t.Errorf("owned secret gone: %v", err)
	}
}

// Databases themselves stay user-managed: apply and delete work, and an
// unowned Workload is still free to change.
func TestDatabaseIsUserManagedAndUnownedObjectsAreFree(t *testing.T) {
	_, c := newVolumeServer(t)
	ctx := context.Background()

	if err := applyObjs(t, c, dbObj("app-db")); err != nil {
		t.Fatalf("apply Database: %v", err)
	}
	if err := applyObjs(t, c, workloadObj("web")); err != nil {
		t.Fatal(err)
	}
	if err := applyObjs(t, c, workloadObj("web")); err != nil {
		t.Fatalf("re-apply of an unowned workload: %v", err)
	}
	if _, err := c.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindDatabase, Name: "app-db"}); err != nil {
		t.Fatalf("delete Database: %v", err)
	}
}
