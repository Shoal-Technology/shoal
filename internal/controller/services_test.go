package controller

import (
	"slices"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
)

func (e *env) service(name, selector string) {
	e.t.Helper()
	spec, err := objects.Encode(&v1alpha1.ServiceSpec{Selector: selector, Ports: []*v1alpha1.ServicePort{{Port: 80, TargetPort: 80}}})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Create(ctx, store.Record{Kind: v1alpha1.KindService, Name: name, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) endpoints(name string) []string {
	e.t.Helper()
	rec, err := e.st.Get(ctx, v1alpha1.KindService, name)
	if err != nil {
		e.t.Fatal(err)
	}
	svc, err := objects.Service(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	return svc.Status.GetEndpoints()
}

func serviceFixture(t *testing.T, policy string) (*env, map[string]string) {
	t.Helper()
	e := newEnv(t)
	e.node("a", v1alpha1.NodeReady, 8000, 8192, 0)
	e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)
	e.workload("web", 2, func(s *v1alpha1.WorkloadSpec) { s.PartitionPolicy = policy })
	e.reconcile()
	ips := map[string]string{"a": "10.42.0.2", "b": "10.42.1.2"}
	for _, r := range e.replicas() {
		ip := ips[r.Spec.Node]
		e.reportStatus(r.Metadata.Name, func(r *v1alpha1.Replica) *v1alpha1.ReplicaStatus {
			return &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaRunning, Ready: true, Ip: ip, TemplateGeneration: r.Spec.TemplateGeneration}
		})
	}
	e.service("web", "web")
	e.reconcile()
	if got := e.endpoints("web"); !slices.Equal(got, []string{ips["a"], ips["b"]}) {
		t.Fatalf("fixture endpoints = %v", got)
	}
	return e, ips
}

func TestServiceWithdrawsNotReadyBeforeEviction(t *testing.T) {
	for _, policy := range []string{"Replace", "Hold"} {
		t.Run(policy, func(t *testing.T) {
			e, ips := serviceFixture(t, policy)
			before := e.replicas()
			e.node("b", v1alpha1.NodeNotReady, 8000, 8192, 25*time.Second)
			e.reconcile()
			if got := e.endpoints("web"); !slices.Equal(got, []string{ips["a"]}) {
				t.Fatalf("NotReady endpoint retained before eviction: %v", got)
			}
			after := e.replicas()
			if len(after) != len(before) {
				t.Fatal("endpoint withdrawal changed replica count")
			}
			for i, r := range after {
				if r.Metadata.Name != before[i].Metadata.Name || r.Metadata.Generation != before[i].Metadata.Generation {
					t.Fatal("endpoint withdrawal replaced or mutated assignment")
				}
			}
			rev := e.st.Revision()
			e.reconcile()
			if e.st.Revision() != rev {
				t.Fatal("endpoint withdrawal is not idempotent")
			}
			e.node("b", v1alpha1.NodeReady, 8000, 8192, 0)
			e.reconcile()
			if got := e.endpoints("web"); !slices.Equal(got, []string{ips["a"], ips["b"]}) {
				t.Fatalf("reconnect did not restore endpoints: %v", got)
			}
		})
	}
}

func TestServiceHoldWithdrawsDeletedNode(t *testing.T) {
	e, ips := serviceFixture(t, "Hold")
	if err := e.st.Delete(ctx, v1alpha1.KindNode, "b", 0); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.endpoints("web"); !slices.Equal(got, []string{ips["a"]}) {
		t.Fatalf("deleted node endpoint retained under Hold: %v", got)
	}
	if len(e.replicas()) != 2 {
		t.Fatal("Hold assignment was evicted")
	}
}

func TestServiceHoldAllNotReadyHasNoEndpoints(t *testing.T) {
	e, _ := serviceFixture(t, "Hold")
	for _, node := range []string{"a", "b"} {
		e.node(node, v1alpha1.NodeNotReady, 8000, 8192, time.Hour)
	}
	e.reconcile()
	if got := e.endpoints("web"); len(got) != 0 {
		t.Fatalf("all uncertain endpoints retained: %v", got)
	}
	if len(e.replicas()) != 2 {
		t.Fatal("Hold assignment was evicted")
	}
}

func TestServiceRequiresReplicaReadinessAndCurrentTemplate(t *testing.T) {
	for _, scenario := range []string{"unready", "stale template", "no IP"} {
		t.Run(scenario, func(t *testing.T) {
			e, ips := serviceFixture(t, "Replace")
			for _, r := range e.replicas() {
				if r.Spec.Node != "b" {
					continue
				}
				e.reportStatus(r.Metadata.Name, func(r *v1alpha1.Replica) *v1alpha1.ReplicaStatus {
					st := r.Status
					switch scenario {
					case "unready":
						st.Ready = false
					case "stale template":
						st.TemplateGeneration++
					case "no IP":
						st.Ip = ""
					}
					return st
				})
			}
			e.reconcile()
			if got := e.endpoints("web"); !slices.Equal(got, []string{ips["a"]}) {
				t.Fatalf("ineligible replica in endpoints: %v", got)
			}
		})
	}
}
