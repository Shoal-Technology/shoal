package build

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Fake is an in-memory Executor for tests. A started job stays Running until
// a test calls Finish. Cancel marks the job Failed with the message
// "cancelled", as a real executor reports it.
type Fake struct {
	// StartErr, when set, is returned by Start and no job is created.
	StartErr error

	mu        sync.Mutex
	jobs      map[string]*fakeJob
	started   []string
	cancelled []string
}

type fakeJob struct {
	status Status
	lines  []Line
}

// NewFake returns an empty fake executor.
func NewFake() *Fake { return &Fake{jobs: map[string]*fakeJob{}} }

// Start records the job as Running. Starting a name twice is an error.
func (f *Fake) Start(_ context.Context, job Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	if _, ok := f.jobs[job.Name]; ok {
		return errors.New("job already started")
	}
	f.jobs[job.Name] = &fakeJob{status: Status{State: Running}}
	f.started = append(f.started, job.Name)
	return nil
}

// Status returns the job's state.
func (f *Fake) Status(_ context.Context, name string) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[name]
	if !ok {
		return Status{}, ErrUnknownJob
	}
	return j.status, nil
}

// Cancel marks a running job Failed ("cancelled") and records the call.
func (f *Fake) Cancel(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[name]
	if !ok {
		return ErrUnknownJob
	}
	if j.status.State == Running {
		j.status = Status{State: Failed, Message: "cancelled"}
	}
	f.cancelled = append(f.cancelled, name)
	return nil
}

// Logs returns the lines appended with Log.
func (f *Fake) Logs(_ context.Context, name string) ([]Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[name]
	if !ok {
		return nil, ErrUnknownJob
	}
	return append([]Line(nil), j.lines...), nil
}

// Finish sets the state of a job, as the builder would report it.
func (f *Fake) Finish(name string, st Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[name]; ok {
		j.status = st
	}
}

// Log appends one line of output to a job.
func (f *Fake) Log(name, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[name]; ok {
		j.lines = append(j.lines, Line{Time: time.Now(), Text: text})
	}
}

// Started returns the names of the jobs started, in order.
func (f *Fake) Started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// Cancelled returns the names of the jobs Cancel was called for, in order.
func (f *Fake) Cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}
