// Package pipeline reads shoal-ci.yml, evaluates its rules and defines the
// seam between the pipeline controller and whatever runs a job (ADR 0022).
// Pass A has the parser, the rules and an in-memory fake runner. No container
// runs in pass A.
package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// ErrUnknownJob means the runner has no job with the name: it was never
// started, or it was lost.
var ErrUnknownJob = errors.New("unknown job")

// Request is one job to run. Name is the Job record's name and identifies the
// job to the runner. Env holds the variables the job runs with; secret values
// are not in it (ADR 0022, section 6).
type Request struct {
	Name string
	Run  string
	Job  *v1alpha1.PipelineJob
	Env  map[string]string
}

// State is the state of a started job.
type State string

const (
	Running   State = "Running"
	Succeeded State = "Succeeded"
	Failed    State = "Failed" // the script ran and exited non-zero, or was killed
	Error     State = "Error"  // the job did not run to an end: an infrastructure failure
)

// Reasons of an Error state. Only these are retried (ADR 0022, section 5).
const (
	ReasonRunnerError = "runner_error" // the runner lost the job
	ReasonNodeLost    = "node_lost"    // the node was marked NotReady
)

// Status is the state of a job. ExitCode is set when the script ended. Reason
// is set with Error. Message is the failure text.
type Status struct {
	State    State
	ExitCode int
	Reason   string
	Message  string
	Node     string
	// Build, Image and Digest are set for a build step (ADR 0022, section 4):
	// the Build it started, and the image and digest that Build pushed.
	Build  string
	Image  string
	Digest string
}

// Line is one line of job output.
type Line struct {
	Time time.Time
	Text string
}

// Runner runs jobs. Implementations must be safe for concurrent use. The
// controller calls them from one goroutine, one pass at a time.
type Runner interface {
	// Start begins the job. An error means the job did not start; it is not a
	// job failure. Starting a job that ended earlier under the same name
	// replaces it (a retry).
	Start(ctx context.Context, req Request) error
	// Status reports a started job, or ErrUnknownJob.
	Status(ctx context.Context, name string) (Status, error)
	// Cancel stops a started job. It succeeds for a job that already ended.
	// ErrUnknownJob means there is no such job.
	Cancel(ctx context.Context, name string) error
	// Logs returns the output of a job so far, or ErrUnknownJob.
	Logs(ctx context.Context, name string) ([]Line, error)
}
