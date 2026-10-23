package resource

import (
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func TestDefaultPipelineFillsRefAndPath(t *testing.T) {
	s := &v1alpha1.PipelineSpec{Repo: "https://github.com/acme/app.git"}
	DefaultPipeline(s)
	if s.Ref != DefaultPipelineRef || s.Path != DefaultPipelinePath {
		t.Fatalf("defaults = %q %q", s.Ref, s.Path)
	}
}

func TestValidatePipeline(t *testing.T) {
	cases := []struct {
		name string
		pl   string
		spec *v1alpha1.PipelineSpec
		want string // substring; empty means valid
	}{
		{"valid", "app", &v1alpha1.PipelineSpec{Repo: "https://github.com/acme/app.git", Ref: "main", Path: "shoal-ci.yml"}, ""},
		{"ssh repo", "app", &v1alpha1.PipelineSpec{Repo: "git@github.com:acme/app.git", Ref: "main", Path: "shoal-ci.yml"}, ""},
		{"name too long", strings.Repeat("a", 41), &v1alpha1.PipelineSpec{Repo: "https://h/x", Ref: "main", Path: "shoal-ci.yml"}, "metadata.name"},
		{"credentials in the URL are refused", "app", &v1alpha1.PipelineSpec{Repo: "https://user:pw@h/x", Ref: "main", Path: "shoal-ci.yml"}, "spec.repo"},
		{"path must stay in the repo", "app", &v1alpha1.PipelineSpec{Repo: "https://h/x", Ref: "main", Path: "../shoal-ci.yml"}, "spec.path"},
		{"absolute path is refused", "app", &v1alpha1.PipelineSpec{Repo: "https://h/x", Ref: "main", Path: "/etc/x.yml"}, "spec.path"},
		{"bad ref", "app", &v1alpha1.PipelineSpec{Repo: "https://h/x", Ref: "a..b", Path: "shoal-ci.yml"}, "spec.ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePipeline(tc.pl, tc.spec)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid spec refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v does not name %q", err, tc.want)
			}
		})
	}
}

func TestJobEnvJobWins(t *testing.T) {
	src := &v1alpha1.PipelineSource{Variables: map[string]string{"A": "run", "B": "run"}}
	job := &v1alpha1.PipelineJob{Variables: map[string]string{"B": "job"}}
	env := JobEnv(src, job)
	if env["A"] != "run" || env["B"] != "job" {
		t.Fatalf("env = %v", env)
	}
	if src.Variables["B"] != "run" {
		t.Fatal("JobEnv must not change the run's variables")
	}
}

func TestPipelineRunTerminalAndJobTerminal(t *testing.T) {
	if JobTerminal(PhaseManual) || !JobTerminal(PhaseSkipped) {
		t.Error("a manual job waits and a skipped job is final")
	}
	if PipelineRunTerminal(PhaseRunning) || !PipelineRunTerminal(PhaseCancelled) {
		t.Error("a running run is not final and a cancelled run is")
	}
}
