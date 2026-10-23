package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// projDoc renders one manifest. spec is the indented body under spec:, and is
// left out when empty.
func projDoc(kind, name, project, spec string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n", v1alpha1.APIVersion, kind, name)
	if project != "" {
		fmt.Fprintf(&b, "  project: %s\n", project)
	}
	if spec != "" {
		b.WriteString("spec:\n" + spec)
	}
	return b.String()
}

// workloadBody is the spec of a Workload with one replica per n and a CPU
// request of cpu.
func workloadBody(replicas int, cpu string) string {
	return fmt.Sprintf("  image: nginx:1.27\n  replicas: %d\n  resources:\n    cpu: %s\n    memory: 64Mi\n", replicas, cpu)
}

// projWantStatus checks that err has the code and a message containing substr.
func projWantStatus(t *testing.T, err error, code codes.Code, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s containing %q, got no error", code, substr)
	}
	st, _ := status.FromError(err)
	if st.Code() != code || !strings.Contains(st.Message(), substr) {
		t.Fatalf("got %s %q, want %s containing %q", st.Code(), st.Message(), code, substr)
	}
}

func workloadNames(resp *v1alpha1.ListResponse) []string {
	var out []string
	for _, o := range resp.GetItems() {
		out = append(out, o.GetWorkload().GetMetadata().GetName())
	}
	slices.Sort(out)
	return out
}

// projectClient wraps the admin client with the calls the tests share.
type projectClient struct {
	t     *testing.T
	admin v1alpha1.ControlClient
	ctx   context.Context
}

func (p projectClient) apply(y string) (string, error) {
	p.t.Helper()
	resp, err := p.admin.Apply(p.ctx, &v1alpha1.ApplyRequest{Objects: decode(p.t, y)})
	if err != nil {
		return "", err
	}
	return resp.GetResults()[0].GetAction(), nil
}

func (p projectClient) mustApply(y string) string {
	p.t.Helper()
	a, err := p.apply(y)
	if err != nil {
		p.t.Fatalf("apply: %v\n%s", err, y)
	}
	return a
}

func TestProjectsScopeObjects(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	ctx := p.ctx

	// The default project exists before anyone applies it (ADR 0023, section 1).
	def, err := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindProject, Name: tenancy.DefaultProject})
	if err != nil || def.GetProject().GetMetadata().GetName() != tenancy.DefaultProject {
		t.Fatalf("default project: %v, %v", def, err)
	}

	if a := p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", "  displayName: Team A\n")); a != "created" {
		t.Fatalf("project apply: %s", a)
	}
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "team-a", workloadBody(1, "100m"))); a != "created" {
		t.Fatalf("web in team-a: %s", a)
	}
	// Without a project, an object is in default. Applied again, it is unchanged:
	// the record has no project label, as before projects existed.
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "api", "", workloadBody(1, "100m"))); a != "created" {
		t.Fatalf("api in default: %s", a)
	}
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "api", "", workloadBody(1, "100m"))); a != "unchanged" {
		t.Fatalf("api re-applied: %s", a)
	}
	// Left out, metadata.project means default: the name in team-a is another
	// object (ADR 0023, pass B1).
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "", workloadBody(1, "100m"))); a != "created" {
		t.Fatalf("web without a project: %s", a)
	}

	web, err := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := web.GetWorkload().GetMetadata().GetProject(); got != "team-a" {
		t.Fatalf("web project = %q, want team-a", got)
	}
	if _, ok := web.GetWorkload().GetMetadata().GetLabels()[tenancy.LabelProject]; ok {
		t.Fatal("the reserved project label leaked into metadata.labels")
	}
	api, err := p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if got := api.GetWorkload().GetMetadata().GetProject(); got != tenancy.DefaultProject {
		t.Fatalf("api project = %q, want default", got)
	}

	// A Get in a project that has no such name finds nothing. A Get with no
	// project finds the name only when one project holds it.
	_, err = p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: "ghost"})
	projWantStatus(t, err, codes.NotFound, "not found in project")
	_, err = p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: "web"})
	projWantStatus(t, err, codes.FailedPrecondition, "several projects")

	inTeamA, err := p.admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindWorkload, Project: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := workloadNames(inTeamA); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("team-a workloads = %v", got)
	}
	inDefault, err := p.admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindWorkload, Project: tenancy.DefaultProject})
	if err != nil {
		t.Fatal(err)
	}
	if got := workloadNames(inDefault); !slices.Equal(got, []string{"api", "web"}) {
		t.Fatalf("default workloads = %v", got)
	}
	all, err := p.admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindWorkload})
	if err != nil {
		t.Fatal(err)
	}
	if got := workloadNames(all); !slices.Equal(got, []string{"api", "web", "web"}) {
		t.Fatalf("all workloads = %v", got)
	}

	// The project must exist.
	_, err = p.apply(projDoc(v1alpha1.KindWorkload, "db", "ghost", workloadBody(1, "100m")))
	projWantStatus(t, err, codes.InvalidArgument, `project "ghost" not found`)

	// The reserved label cannot be set by hand.
	reserved := fmt.Sprintf("apiVersion: %s\nkind: Workload\nmetadata:\n  name: x\n  labels:\n    %s: team-b\nspec:\n  image: nginx:1.27\n",
		v1alpha1.APIVersion, tenancy.LabelProject)
	_, err = p.apply(reserved)
	projWantStatus(t, err, codes.InvalidArgument, "is reserved")

	// A project with objects cannot be deleted, and the wrong project finds
	// nothing to delete.
	_, err = p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindProject, Name: "team-a"})
	projWantStatus(t, err, codes.FailedPrecondition, "still has 1 objects")
	_, err = p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: "ghost"})
	projWantStatus(t, err, codes.NotFound, "not found in project")
	if _, err := p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindWorkload, Name: "web", Project: tenancy.DefaultProject}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindProject, Name: "team-a"}); err != nil {
		t.Fatal(err)
	}
	_, err = p.admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindProject, Name: tenancy.DefaultProject})
	projWantStatus(t, err, codes.FailedPrecondition, "cannot be deleted")

	projects, err := p.admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindProject})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range projects.GetItems() {
		names = append(names, o.GetProject().GetMetadata().GetName())
	}
	if !slices.Equal(names, []string{tenancy.DefaultProject}) {
		t.Fatalf("projects after delete = %v", names)
	}
}

func TestResourceQuotaRefusesGrowth(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	ctx := p.ctx

	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	p.mustApply(projDoc(v1alpha1.KindResourceQuota, "compute", "team-a", "  cpu: \"1\"\n  workloads: 1\n  storage: 10Gi\n"))

	p.mustApply(projDoc(v1alpha1.KindWorkload, "a", "team-a", workloadBody(1, "500m")))

	// A second Workload exceeds the count of 1.
	_, err := p.apply(projDoc(v1alpha1.KindWorkload, "b", "team-a", workloadBody(1, "500m")))
	projWantStatus(t, err, codes.ResourceExhausted, `quota "compute" in project "team-a": workloads would use 2`)

	// Replicas raise CPU past the limit of 1 core.
	_, err = p.apply(projDoc(v1alpha1.KindWorkload, "a", "team-a", workloadBody(3, "500m")))
	projWantStatus(t, err, codes.ResourceExhausted, "cpu would use 1500m (now 500m), hard limit is 1")

	// A change that lowers usage is always accepted.
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "a", "team-a", workloadBody(1, "250m"))); a != "configured" {
		t.Fatalf("lowering usage: %s", a)
	}

	// Projects without a quota are not limited.
	p.mustApply(projDoc(v1alpha1.KindWorkload, "c", "", workloadBody(1, "500m")))
	p.mustApply(projDoc(v1alpha1.KindWorkload, "d", "", workloadBody(1, "500m")))

	// Storage: 8Gi fits in 10Gi, another 4Gi does not, and nothing is written.
	p.mustApply(projDoc(v1alpha1.KindVolume, "data", "team-a", "  size: 8Gi\n"))
	_, err = p.apply(projDoc(v1alpha1.KindVolume, "logs", "team-a", "  size: 4Gi\n"))
	projWantStatus(t, err, codes.ResourceExhausted, `quota "compute" in project "team-a": storage would use 12Gi (now 8Gi), hard limit is 10Gi`)
	_, err = p.admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindVolume, Name: "logs"})
	projWantStatus(t, err, codes.NotFound, "not found")
}

func TestNewRejectsUnknownAuthzMode(t *testing.T) {
	_, err := New(context.Background(), Config{DataDir: t.TempDir(), AuthzMode: "log"})
	if err == nil || !strings.Contains(err.Error(), "authorization mode") {
		t.Fatalf("New with an unknown mode: %v", err)
	}
}

func TestAdminKeyIsAllowedInEnforceMode(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, func(c *Config) { c.AuthzMode = "enforce" })
	p := projectClient{t: t, admin: ts.admin(t), ctx: context.Background()}
	p.mustApply(projDoc(v1alpha1.KindProject, "team-a", "", ""))
	if a := p.mustApply(projDoc(v1alpha1.KindWorkload, "web", "team-a", workloadBody(1, "100m"))); a != "created" {
		t.Fatalf("admin in enforce mode: %s", a)
	}
}
