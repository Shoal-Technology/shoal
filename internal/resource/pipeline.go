package resource

import (
	"errors"
	"fmt"
	"maps"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Pipelines (ADR 0022, pass A). A Pipeline is the definition for one
// repository; a PipelineRun is one execution of its shoal-ci.yml; a Job is one
// job of one run.
const (
	// PipelineMaxNameLen keeps "<pipeline>-<suffix>" short enough to read.
	PipelineMaxNameLen = 40
	// PipelineRunSuffixLen is the length of a run's random suffix.
	PipelineRunSuffixLen = 5
	// DefaultPipelineRef is the ref manual runs read when a Pipeline names none.
	DefaultPipelineRef = "HEAD"
	// DefaultPipelinePath is the file read when a Pipeline names none.
	DefaultPipelinePath = "shoal-ci.yml"

	// TriggerManual is the trigger of a run started with StartPipelineRun.
	TriggerManual = "manual"
	// TrustProtected is the trust of a manual run: only the admin key starts
	// one (ADR 0022, section 7).
	TrustProtected = "protected"
)

// Phases of PipelineRun and Job (ADR 0022, sections 4 and 12). The names
// follow the Build phases. Manual and Skipped are Job phases only.
const (
	PhaseNew       = "New"
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseManual    = "Manual"
	PhaseComplete  = "Complete"
	PhaseFailed    = "Failed"
	PhaseError     = "Error"
	PhaseCancelled = "Cancelled"
	PhaseSkipped   = "Skipped"
)

// PipelineRunTerminal reports whether a run's phase is final.
func PipelineRunTerminal(phase string) bool {
	switch phase {
	case PhaseComplete, PhaseFailed, PhaseError, PhaseCancelled:
		return true
	}
	return false
}

// JobTerminal reports whether a job's phase is final. A Manual job is not
// final: it waits for PlayJob.
func JobTerminal(phase string) bool {
	switch phase {
	case PhaseComplete, PhaseFailed, PhaseError, PhaseCancelled, PhaseSkipped:
		return true
	}
	return false
}

// PipelineRunName returns the name of a run of pipeline, given its suffix.
func PipelineRunName(pipeline, suffix string) string { return pipeline + "-" + suffix }

// JobName returns the name of the job job of run run. The name is the record
// name and the name the runner knows the job by.
func JobName(run, job string) string { return run + "-" + job }

// DefaultPipeline fills in the defaults of a Pipeline spec.
func DefaultPipeline(s *v1alpha1.PipelineSpec) {
	if s.Ref == "" {
		s.Ref = DefaultPipelineRef
	}
	if s.Path == "" {
		s.Path = DefaultPipelinePath
	}
}

// ValidatePipeline checks a defaulted Pipeline spec and reports every problem,
// each prefixed with its field path. The clone Secret is checked when a run
// starts, as a build's is (ADR 0021).
func ValidatePipeline(name string, s *v1alpha1.PipelineSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, PipelineMaxNameLen) {
		bad("metadata.name", "must be a lowercase DNS label of at most %d characters", PipelineMaxNameLen)
	}
	if err := checkGitURL(s.GetRepo()); err != nil {
		bad("spec.repo", "%v", err)
	}
	if r := s.GetRef(); !validGitRef(r) {
		bad("spec.ref", "%q is not a branch, tag or commit name", r)
	}
	if err := checkRelPath(s.GetPath()); err != nil {
		bad("spec.path", "%v", err)
	}
	if c := s.GetCredentialsSecret(); c != "" && !ValidName(c, 63) {
		bad("spec.credentialsSecret", "invalid Secret name %q", c)
	}
	return errors.Join(errs...)
}

// JobEnv returns the variables a job runs with: the run's, then the job's,
// which win. Secret values are never part of it.
func JobEnv(src *v1alpha1.PipelineSource, job *v1alpha1.PipelineJob) map[string]string {
	env := maps.Clone(src.GetVariables())
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, job.GetVariables())
	return env
}
