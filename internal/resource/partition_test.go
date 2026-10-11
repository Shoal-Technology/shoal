package resource

import (
	"github.com/kaiserern/shoal/api/v1alpha1"
	"testing"
)

func TestPartitionPolicyValidation(t *testing.T) {
	for _, policy := range []string{"", "Hold", "Replace", "hold", "Fence", "Typo"} {
		t.Run(policy, func(t *testing.T) {
			w := &v1alpha1.Workload{Metadata: &v1alpha1.ObjectMeta{Name: "web"}, Spec: &v1alpha1.WorkloadSpec{Image: "nginx", PartitionPolicy: policy}}
			DefaultWorkload(w)
			err := ValidateWorkload(w)
			valid := policy == "" || policy == "Hold" || policy == "Replace"
			if valid != (err == nil) {
				t.Fatalf("policy %q validation = %v", policy, err)
			}
		})
	}
}
