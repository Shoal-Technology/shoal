// Package build is the seam between the Build controller and whatever runs a
// build (ADR 0021). Pass A defines the interface and an in-memory fake for
// tests. Pass B adds the BuildKit executor that runs on builder nodes.
package build

import (
	"context"
	"errors"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// ErrUnknownJob means the executor has no job with the name: it was never
// started, or it was lost.
var ErrUnknownJob = errors.New("unknown build job")

// Job is one build to run. Name is the Build's name and identifies the job.
// Source is the BuildConfig spec as it was when the run started, with defaults
// applied.
type Job struct {
	Name   string
	Config string
	Source *v1alpha1.BuildConfigSpec
}

// State is the state of a started job.
type State string

const (
	Running   State = "Running"
	Succeeded State = "Succeeded"
	Failed    State = "Failed"
)

// Status is the state of a job. Commit, Image and Digest are set when the
// job succeeds; Message is the failure reason when it fails.
type Status struct {
	State   State
	Commit  string // the commit the build resolved to
	Image   string // repository:tag that was pushed
	Digest  string // manifest digest of the pushed image
	Message string
}

// Line is one line of build output.
type Line struct {
	Time time.Time
	Text string
}

// Executor runs builds. Implementations must be safe for concurrent use. The
// controller calls them from one goroutine, one pass at a time.
type Executor interface {
	// Start begins the job. An error means the job did not start; it is not a
	// build failure.
	Start(ctx context.Context, job Job) error
	// Status reports a started job, or ErrUnknownJob.
	Status(ctx context.Context, name string) (Status, error)
	// Cancel stops a started job. It succeeds for a job that already ended.
	// ErrUnknownJob means there is no such job.
	Cancel(ctx context.Context, name string) error
	// Logs returns the output of a job so far, or ErrUnknownJob.
	Logs(ctx context.Context, name string) ([]Line, error)
}
