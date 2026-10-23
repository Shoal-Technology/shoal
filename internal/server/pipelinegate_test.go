package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

// gateEnv returns the Control service over a store with a protected environment
// production and one job of run1 that waits at its gate.
func gateEnv(t *testing.T, phase string) (*controlService, store.Store) {
	t.Helper()
	ctx := t.Context()
	st := memory.New(time.Now)
	put := func(kind, name string, spec, st2 proto.Message) {
		t.Helper()
		b, err := objects.Encode(spec)
		if err != nil {
			t.Fatal(err)
		}
		var s []byte
		if st2 != nil {
			if s, err = objects.Encode(st2); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.Create(ctx, store.Record{Kind: kind, Name: name, Spec: b, Status: s}); err != nil {
			t.Fatal(err)
		}
	}
	put(v1alpha1.KindEnvironment, "production", &v1alpha1.EnvironmentSpec{Protected: true, RequiredApprovals: 1, Workloads: []string{"web"}}, nil)
	put(v1alpha1.KindJob, "run1-deploy", &v1alpha1.JobSpec{
		PipelineRun: "run1",
		Job:         &v1alpha1.PipelineJob{Name: "deploy", Environment: &v1alpha1.JobEnvironment{Name: "production"}},
	}, &v1alpha1.JobStatus{Phase: phase})
	srv := &Server{store: st, log: slog.Default()}
	return &controlService{s: srv}, st
}

// asAdmin is a context whose caller is the admin certificate.
func asAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey{}, caller{id: security.Identity{Name: "admin", Role: security.RoleAdmin}})
}

func jobSpecOf(t *testing.T, st store.Store) *v1alpha1.JobSpec {
	t.Helper()
	rec, err := st.Get(t.Context(), v1alpha1.KindJob, "run1-deploy")
	if err != nil {
		t.Fatal(err)
	}
	j, err := objects.Job(rec)
	if err != nil {
		t.Fatal(err)
	}
	return j.GetSpec()
}

func TestApproveJobRecordsTheCallerOnce(t *testing.T) {
	c, st := gateEnv(t, resource.PhaseManual)
	ctx := asAdmin(t.Context())
	if _, err := c.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: "run1-deploy", Comment: "ship it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: "run1-deploy"}); err != nil {
		t.Fatalf("a repeat approval by the same caller: %v", err)
	}
	as := jobSpecOf(t, st).GetApprovals()
	if len(as) != 1 {
		t.Fatalf("approvals = %v, want one", as)
	}
	if a := as[0]; a.GetBy() != "admin" || a.GetDecision() != resource.DecisionApproved || a.GetComment() != "ship it" || a.GetAt() == nil {
		t.Fatalf("approval = %v, want approved by admin with the comment and a time", a)
	}
}

func TestRejectedJobCannotBeDecidedAgain(t *testing.T) {
	c, st := gateEnv(t, resource.PhaseManual)
	ctx := asAdmin(t.Context())
	if _, err := c.RejectJob(ctx, &v1alpha1.RejectJobRequest{Job: "run1-deploy", Comment: "freeze"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: "run1-deploy"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("approve after reject: %v, want FailedPrecondition", err)
	}
	if n := len(jobSpecOf(t, st).GetApprovals()); n != 1 {
		t.Fatalf("approvals = %d, want only the rejection", n)
	}
}

func TestOnlyAGatedJobWaitingCanBeDecided(t *testing.T) {
	c, _ := gateEnv(t, resource.PhaseRunning)
	ctx := asAdmin(t.Context())
	if _, err := c.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: "run1-deploy"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("approve of a running job: %v, want FailedPrecondition", err)
	}
	if _, err := c.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: "run1-deploy", Comment: string(make([]byte, maxApprovalComment+1))}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a long comment: %v, want InvalidArgument", err)
	}
}

func TestEnvironmentsOfAFileAreChecked(t *testing.T) {
	c, _ := gateEnv(t, resource.PhaseManual)
	ctx := t.Context()
	deploy := func(env, workload string) *v1alpha1.PipelineSource {
		return &v1alpha1.PipelineSource{Jobs: []*v1alpha1.PipelineJob{{
			Name: "deploy", Stage: "deploy", Environment: &v1alpha1.JobEnvironment{Name: env},
			Deploy: &v1alpha1.JobDeploy{Workload: workload, From: "build"},
		}}}
	}
	if err := c.s.checkPipelineEnvironments(ctx, "", deploy("production", "web"), resource.TrustProtected); err != nil {
		t.Fatalf("a deploy of a listed workload: %v", err)
	}
	if status.Code(c.s.checkPipelineEnvironments(ctx, "", deploy("production", "api"), resource.TrustProtected)) != codes.FailedPrecondition {
		t.Fatal("a workload the environment does not list was accepted")
	}
	if status.Code(c.s.checkPipelineEnvironments(ctx, "", deploy("staging", "web"), resource.TrustProtected)) != codes.FailedPrecondition {
		t.Fatal("an environment that does not exist was accepted")
	}
	// An environment of another project is not the run's: it does not exist here.
	if status.Code(c.s.checkPipelineEnvironments(ctx, "team-a", deploy("production", "web"), resource.TrustProtected)) != codes.FailedPrecondition {
		t.Fatal("an environment of another project was accepted")
	}
	// A same-repo run never reaches an environment: its jobs are created Skipped.
	if err := c.s.checkPipelineEnvironments(ctx, "", deploy("staging", "web"), resource.TrustSameRepo); err != nil {
		t.Fatalf("a same-repo run was checked against environments: %v", err)
	}
}
