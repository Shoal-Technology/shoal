package resource

import (
	"slices"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func TestDefaultEnvironmentProtectedNeedsOneApproval(t *testing.T) {
	s := &v1alpha1.EnvironmentSpec{Protected: true}
	DefaultEnvironment(s)
	if s.GetRequiredApprovals() != 1 {
		t.Fatalf("required approvals = %d, want 1 for a protected environment", s.GetRequiredApprovals())
	}
	open := &v1alpha1.EnvironmentSpec{}
	DefaultEnvironment(open)
	if open.GetRequiredApprovals() != 0 {
		t.Fatalf("an open environment got %d approvals, want 0", open.GetRequiredApprovals())
	}
}

func TestValidateEnvironment(t *testing.T) {
	cases := []struct {
		name string
		spec *v1alpha1.EnvironmentSpec
		ok   bool
	}{
		{"production", &v1alpha1.EnvironmentSpec{Url: "https://app.example.com", Workloads: []string{"web"}}, true},
		{"Production", &v1alpha1.EnvironmentSpec{}, false},
		{"staging", &v1alpha1.EnvironmentSpec{Url: "ftp://app.example.com"}, false},
		{"staging", &v1alpha1.EnvironmentSpec{Url: "not a url"}, false},
		{"staging", &v1alpha1.EnvironmentSpec{RequiredApprovals: MaxRequiredApprovals + 1}, false},
		{"staging", &v1alpha1.EnvironmentSpec{RequiredApprovals: -1}, false},
		{"staging", &v1alpha1.EnvironmentSpec{Workloads: []string{"Bad_Name"}}, false},
	}
	for _, c := range cases {
		err := ValidateEnvironment(c.name, c.spec)
		if (err == nil) != c.ok {
			t.Errorf("ValidateEnvironment(%q, %v) = %v, want ok=%v", c.name, c.spec, err, c.ok)
		}
	}
}

func TestJobGatedAndApprovalsNeeded(t *testing.T) {
	plain := &v1alpha1.PipelineJob{Name: "deploy"}
	withEnv := &v1alpha1.PipelineJob{Name: "deploy", Environment: &v1alpha1.JobEnvironment{Name: "production"}}
	withGate := &v1alpha1.PipelineJob{Name: "deploy", Environment: &v1alpha1.JobEnvironment{Name: "production", Approval: ApprovalRequired}}
	open := &v1alpha1.EnvironmentSpec{}
	protected := &v1alpha1.EnvironmentSpec{Protected: true, RequiredApprovals: 2}

	if JobGated(plain, protected) {
		t.Error("a job with no environment is gated")
	}
	if JobGated(withEnv, open) {
		t.Error("an environment that is open and not asked for approval gated its job")
	}
	if !JobGated(withGate, open) {
		t.Error("approval: required did not gate the job")
	}
	if !JobGated(withEnv, protected) {
		t.Error("a protected environment did not gate its job")
	}
	if n := ApprovalsNeeded(true, protected); n != 2 {
		t.Errorf("ApprovalsNeeded = %d, want 2", n)
	}
	if n := ApprovalsNeeded(true, open); n != 1 {
		t.Errorf("a gated job with no count needs %d, want 1", n)
	}
	if n := ApprovalsNeeded(false, protected); n != 0 {
		t.Errorf("an ungated job needs %d approvals, want 0", n)
	}
}

func TestApprovedByCountsEachApproverOnce(t *testing.T) {
	approvals := []*v1alpha1.JobApproval{
		{Decision: DecisionApproved, By: "admin"},
		{Decision: DecisionApproved, By: "admin"},
		{Decision: DecisionApproved, By: "ops"},
	}
	if got := ApprovedBy(approvals); !slices.Equal(got, []string{"admin", "ops"}) {
		t.Fatalf("ApprovedBy = %v, want [admin ops]", got)
	}
	if Rejection(approvals) != nil {
		t.Fatal("an approval is not a rejection")
	}
	approvals = append(approvals, &v1alpha1.JobApproval{Decision: DecisionRejected, By: "lead"})
	if r := Rejection(approvals); r == nil || r.GetBy() != "lead" {
		t.Fatalf("Rejection = %v, want the rejection by lead", r)
	}
}

func TestBuildOutputKey(t *testing.T) {
	if got, want := BuildOutputKey("build-web-image", "IMAGE"), "SHOAL_BUILD_BUILD_WEB_IMAGE_IMAGE"; got != want {
		t.Fatalf("BuildOutputKey = %q, want %q", got, want)
	}
	if got, want := BuildOutputKey("api", "DIGEST"), "SHOAL_BUILD_API_DIGEST"; got != want {
		t.Fatalf("BuildOutputKey = %q, want %q", got, want)
	}
}

func TestProtectedBranchesDefaultToMain(t *testing.T) {
	s := &v1alpha1.PipelineSpec{Repo: "https://example.com/acme/web.git"}
	DefaultPipeline(s)
	if got := ProtectedBranchesOf(s); !slices.Equal(got, []string{"main"}) {
		t.Fatalf("default protected branches = %v, want [main]", got)
	}
	s.ProtectedBranches = []string{"release", "main"}
	if got := ProtectedBranchesOf(s); !slices.Equal(got, []string{"release", "main"}) {
		t.Fatalf("protected branches = %v", got)
	}
	if TrustOfPush("release", ProtectedBranchesOf(s)) != TrustProtected {
		t.Fatal("a push to a named protected branch is not protected")
	}
	if TrustOfPush("feature", ProtectedBranchesOf(s)) != TrustSameRepo {
		t.Fatal("a push to another branch is not same-repo")
	}
}
