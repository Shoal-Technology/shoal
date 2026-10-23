package tenancy

import (
	"maps"
	"testing"
)

func TestProjectOfDefaultsWithoutLabel(t *testing.T) {
	if got := ProjectOf(nil); got != DefaultProject {
		t.Fatalf("nil labels: got %q, want %q", got, DefaultProject)
	}
	if got := ProjectOf(map[string]string{"app": "blog"}); got != DefaultProject {
		t.Fatalf("labels without project: got %q, want %q", got, DefaultProject)
	}
	if got := ProjectOf(map[string]string{LabelProject: "team-a"}); got != "team-a" {
		t.Fatalf("labelled: got %q, want team-a", got)
	}
}

func TestWithProjectKeepsDefaultObjectsUnlabelled(t *testing.T) {
	// A default object's labels are exactly what the user wrote, so an
	// unchanged apply stays unchanged (ADR 0023, section 2).
	in := map[string]string{"app": "blog"}
	got := WithProject(in, DefaultProject)
	if !maps.Equal(got, in) {
		t.Fatalf("default project changed labels: %v", got)
	}
	if got := WithProject(nil, DefaultProject); got != nil {
		t.Fatalf("empty default labels: got %v, want nil", got)
	}
}

func TestWithProjectSetsAndReplaces(t *testing.T) {
	in := map[string]string{"app": "blog", LabelProject: "stale"}
	got := WithProject(in, "team-a")
	want := map[string]string{"app": "blog", LabelProject: "team-a"}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if in[LabelProject] != "stale" {
		t.Fatalf("WithProject modified its input: %v", in)
	}
	// Moving an object back to default removes the label.
	back := WithProject(got, DefaultProject)
	if _, ok := back[LabelProject]; ok {
		t.Fatalf("default project kept the label: %v", back)
	}
}

func TestWithoutProjectStripsOnlyTheReservedLabel(t *testing.T) {
	got := WithoutProject(map[string]string{"app": "blog", LabelProject: "team-a"})
	if !maps.Equal(got, map[string]string{"app": "blog"}) {
		t.Fatalf("got %v", got)
	}
	if got := WithoutProject(map[string]string{LabelProject: "team-a"}); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestProjectScoped(t *testing.T) {
	for _, kind := range []string{"Workload", "Service", "Route", "Volume", "Database", "BuildConfig", "Build", "Secret", "Config", "ResourceQuota"} {
		if !ProjectScoped(kind) {
			t.Errorf("%s should be project-scoped", kind)
		}
	}
	for _, kind := range []string{"Node", "Replica", "Project", "JoinToken", "Certificate", "RouteClaim"} {
		if ProjectScoped(kind) {
			t.Errorf("%s should be cluster-wide", kind)
		}
	}
}

func TestValidName(t *testing.T) {
	long := ""
	for range 64 {
		long += "a"
	}
	cases := map[string]bool{
		"team-a":  true,
		"a":       true,
		"default": true,
		"Team":    false,
		"-team":   false,
		"team-":   false,
		"team_a":  false,
		"":        false,
		long:      false,
	}
	for name, want := range cases {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
	if err := CheckName("Team"); err == nil {
		t.Fatal("CheckName accepted an upper-case name")
	}
}
