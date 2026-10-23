package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func TestBuildCreateConfigTakesOnlyBuildConfigs(t *testing.T) {
	_, err := buildConfigObjects([]byte("kind: Workload\nmetadata:\n  name: web\nspec:\n  image: nginx\n"))
	if err == nil || !strings.Contains(err.Error(), "not a BuildConfig") {
		t.Fatalf("want a refusal for a Workload, got %v", err)
	}
}

func TestBuildCreateConfigDecodesBuildArgs(t *testing.T) {
	objs, err := buildConfigObjects([]byte(`kind: BuildConfig
metadata:
  name: web
spec:
  git:
    url: https://github.com/acme/web.git
    ref: main
  buildArgs:
    VERSION: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects = %d", len(objs))
	}
	spec := objs[0].GetBuildConfig().GetSpec()
	if spec.GetBuildArgs()["VERSION"] != "1" || spec.GetGit().GetRef() != "main" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestBuildCreateConfigRejectsInvalidSpecLocally(t *testing.T) {
	_, err := buildConfigObjects([]byte(`kind: BuildConfig
metadata:
  name: web
spec:
  git:
    url: http://example.com/web.git
`))
	if err == nil || !strings.Contains(err.Error(), "spec.git.url") {
		t.Fatalf("want a spec.git.url error, got %v", err)
	}
}

func TestBuildCommandArgs(t *testing.T) {
	cases := []struct {
		cmd  *cobra.Command
		args []string
		ok   bool
	}{
		{newBuildStartCmd(), nil, false},
		{newBuildStartCmd(), []string{"web"}, true},
		{newBuildStartCmd(), []string{"web", "api"}, false},
		{newBuildLsCmd(), nil, true},
		{newBuildLsCmd(), []string{"web"}, true},
		{newBuildLsCmd(), []string{"web", "api"}, false},
		{newBuildLogsCmd(), nil, false},
		{newBuildLogsCmd(), []string{"web-abcde"}, true},
		{newBuildCancelCmd(), nil, false},
		{newBuildCancelCmd(), []string{"web-abcde"}, true},
	}
	for _, tc := range cases {
		err := tc.cmd.Args(tc.cmd, tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("%s %v: ok=%v, got %v", tc.cmd.Name(), tc.args, tc.ok, err)
		}
	}
}

func buildObject(name, config string, created time.Time, phase string) *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Build{Build: &v1alpha1.Build{
		Metadata: &v1alpha1.ObjectMeta{Name: name, CreatedAt: timestamppb.New(created)},
		Spec:     &v1alpha1.BuildSpec{Config: config},
		Status:   &v1alpha1.BuildStatus{Phase: phase},
	}}}
}

func TestFilterBuildsKeepsOneConfig(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	items := []*v1alpha1.Object{
		buildObject("web-aaaaa", "web", now, "Complete"),
		buildObject("api-bbbbb", "api", now, "Running"),
		{Kind: &v1alpha1.Object_Workload{Workload: &v1alpha1.Workload{}}},
	}
	if got := filterBuilds(items, "web"); len(got) != 1 || got[0].GetBuild().GetMetadata().GetName() != "web-aaaaa" {
		t.Fatalf("filter web = %v", got)
	}
	if got := filterBuilds(items, ""); len(got) != 2 {
		t.Fatalf("filter all = %d builds, want 2", len(got))
	}
}

func TestPrintBuildsNewestFirst(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	older := buildObject("web-aaaaa", "web", now.Add(-2*time.Hour), "Failed")
	newer := buildObject("web-bbbbb", "web", now.Add(-time.Minute), "Complete")
	newer.GetBuild().GetStatus().Commit = "0123456789abcdef0123456789abcdef01234567"
	newer.GetBuild().GetStatus().Image = "web:0123456"

	var buf bytes.Buffer
	if err := printBuilds(&buf, []*v1alpha1.Object{older, newer}, now); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[1], "web-bbbbb") || !strings.HasPrefix(lines[2], "web-aaaaa") {
		t.Fatalf("order wrong:\n%s", buf.String())
	}
	if !strings.Contains(lines[1], "0123456789ab") || !strings.Contains(lines[1], "web:0123456") || !strings.Contains(lines[1], "Complete") {
		t.Fatalf("row missing fields: %s", lines[1])
	}
}
