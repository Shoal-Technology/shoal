package controller

import (
	"github.com/kaiserern/shoal/api/v1alpha1"
	"testing"
	"time"
)

func TestHoldRetainsLostAssignment(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "silent", true: "deleted"}[deleted], func(t *testing.T) {
			e := newEnv(t)
			e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
			e.workload("critical", 1, func(s *v1alpha1.WorkloadSpec) { s.PartitionPolicy = "Hold" })
			e.reconcile()
			e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)
			if deleted {
				if err := e.st.Delete(ctx, v1alpha1.KindNode, "a", 0); err != nil {
					t.Fatal(err)
				}
			} else {
				e.node("a", v1alpha1.NodeNotReady, 8000, 8192, time.Hour)
			}
			e.reconcile()
			if p := e.placement(); p["a"] != 1 || len(p) != 1 {
				t.Fatalf("Hold moved uncertain assignment: %v", p)
			}
			// Spec updates must not replace uncertain assignments either.
			e.workload("critical", 1, func(s *v1alpha1.WorkloadSpec) { s.PartitionPolicy = "Hold"; s.Image = "nginx:1.28" })
			e.reconcile()
			if p := e.placement(); p["a"] != 1 || len(p) != 1 {
				t.Fatalf("update moved uncertain assignment: %v", p)
			}
		})
	}
}
