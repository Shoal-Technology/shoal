package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// TestStartPipelineRunChecksFileAndSecrets covers what StartPipelineRun refuses
// before a run exists: a file that does not parse, a Secret a job names that is
// missing or lacks the key, and a request without a commit. The Secret's value
// is never part of the run.
func TestStartPipelineRunChecksFileAndSecrets(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	apply := func(manifest string) {
		t.Helper()
		objs, err := resource.Decode([]byte(manifest))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: objs}); err != nil {
			t.Fatal(err)
		}
	}
	apply("kind: Pipeline\nmetadata:\n  name: app\nspec:\n  repo: https://github.com/acme/app.git\n")

	const file = "stages: [test]\nunit:\n  stage: test\n  image: alpine\n  script: [make]\n" +
		"  secrets:\n    TOKEN: { secret: npm, key: token }\n"
	start := func(file, commit string) (*v1alpha1.StartPipelineRunResponse, error) {
		return admin.StartPipelineRun(ctx, &v1alpha1.StartPipelineRunRequest{Pipeline: "app", Commit: commit, File: file})
	}

	if _, err := start(file, ""); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no commit: code %v, want InvalidArgument (%v)", status.Code(err), err)
	}
	if _, err := start("stages: [test]\ninclude: other.yml\n", "abc"); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), `"include" is not supported`) {
		t.Errorf("a refused key: %v", err)
	}
	if _, err := start(file, "abc"); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), `names Secret "npm", which does not exist`) {
		t.Errorf("a missing Secret: %v", err)
	}
	apply("kind: Secret\nmetadata:\n  name: npm\nspec:\n  data:\n    other: x\n")
	if _, err := start(file, "abc"); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), `Secret "npm" has no key "token"`) {
		t.Errorf("a Secret without the key: %v", err)
	}
	apply("kind: Secret\nmetadata:\n  name: npm\nspec:\n  data:\n    token: abc123\n")
	resp, err := start(file, "abc")
	if err != nil {
		t.Fatalf("a valid run: %v", err)
	}
	if !strings.HasPrefix(resp.GetRun(), "app-") {
		t.Errorf("run name = %q", resp.GetRun())
	}
	obj, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindPipelineRun, Name: resp.GetRun()})
	if err != nil {
		t.Fatal(err)
	}
	spec := obj.GetPipelineRun().GetSpec()
	if spec.GetSource().GetJobs()[0].GetName() != "unit" || spec.GetCommit() != "abc" || spec.GetTrust() != resource.TrustProtected {
		t.Errorf("run spec = %v", spec)
	}
	if strings.Contains(spec.String(), "abc123") {
		t.Error("the Secret value must not be part of the run")
	}
}
