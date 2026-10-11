package deploy

import (
	"testing"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/appspec"
	"example.com/shoal/internal/resource"
)

// TestOwnedChildrenAreNotOrphans: a Database's children (its Service, Workload,
// credentials Secret and data Volume) carry app=<name> so that the app listing
// shows them, but the Database controller owns them and the manifest never
// names them. Deploy must not report them as orphans, and --prune must not
// delete them. Found by deploy-e2e run 38140939413.
func TestOwnedChildrenAreNotOrphans(t *testing.T) {
	child := &v1alpha1.Object{Kind: &v1alpha1.Object_Service{Service: &v1alpha1.Service{
		Metadata: &v1alpha1.ObjectMeta{Name: "blog-db", Labels: map[string]string{
			appspec.LabelApp:      "blog",
			resource.OwnedByLabel: "database",
		}},
	}}}
	ref, _, _ := Describe(child)
	live := Live{ref: child}
	if got := orphanRefs("blog", nil, live); len(got) != 0 {
		t.Fatalf("orphanRefs = %v, want none for a Database-owned Service", got)
	}
}

// TestUnownedAppObjectIsOrphan: the same object without the owned-by marker is
// an ordinary app object that the manifest dropped, so it is an orphan.
func TestUnownedAppObjectIsOrphan(t *testing.T) {
	obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Service{Service: &v1alpha1.Service{
		Metadata: &v1alpha1.ObjectMeta{Name: "old", Labels: map[string]string{appspec.LabelApp: "blog"}},
	}}}
	ref, _, _ := Describe(obj)
	if got := orphanRefs("blog", nil, Live{ref: obj}); len(got) != 1 {
		t.Fatalf("orphanRefs = %v, want the dropped app object", got)
	}
}
