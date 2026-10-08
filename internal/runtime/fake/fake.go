// Package fake provides an in-memory runtime.Runtime for tests.
package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"example.com/shoal/internal/runtime"
)

// Runtime is an in-memory, concurrency-safe runtime.Runtime.
// Mutating calls are recorded as "op id" strings (e.g. "start web-1").
type Runtime struct {
	mu         sync.Mutex
	now        func() time.Time
	images     map[string]bool
	containers map[string]*container
	failures   map[string]error // key: "op id" or "op *"
	held       map[string]chan struct{}
	crashing   map[string]bool               // images whose containers exit right after starting
	logs       map[string][]runtime.LogEntry // by container ID
	calls      []string
}

type container struct {
	spec   runtime.ContainerSpec
	status runtime.ContainerStatus
}

var _ runtime.Runtime = (*Runtime)(nil)

// New returns an empty fake runtime using now as its clock.
func New(now func() time.Time) *Runtime {
	if now == nil {
		now = time.Now
	}
	return &Runtime{
		now:        now,
		images:     map[string]bool{},
		containers: map[string]*container{},
		failures:   map[string]error{},
		held:       map[string]chan struct{}{},
		crashing:   map[string]bool{},
		logs:       map[string][]runtime.LogEntry{},
	}
}

// CrashImage makes containers of ref exit with code 1 right after every
// start, like an app that crashes on startup.
func (r *Runtime) CrashImage(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.crashing[ref] = true
}

// Spec returns the spec container id was created with.
func (r *Runtime) Spec(id string) (runtime.ContainerSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return runtime.ContainerSpec{}, false
	}
	return c.spec, true
}

// SetLogs sets the log lines Logs returns for container id.
func (r *Runtime) SetLogs(id string, entries []runtime.LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs[id] = slices.Clone(entries)
}

// HoldPull makes pulls of ref block until release is called (or the pull's
// context ends), to simulate a slow registry.
func (r *Runtime) HoldPull(ref string) (release func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	r.held[ref] = ch
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.held, ref)
			r.mu.Unlock()
			close(ch)
		})
	}
}

// AddContainer inserts a container in the given state, bypassing Create.
func (r *Runtime) AddContainer(spec runtime.ContainerSpec, st runtime.ContainerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st.ID, st.Image = spec.ID, spec.Image
	st.Labels = maps.Clone(spec.Labels)
	r.containers[spec.ID] = &container{spec: spec, status: st}
}

// Exit simulates the container's process exiting with code.
func (r *Runtime) Exit(id string, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.containers[id]; ok {
		c.status.State = runtime.StateExited
		c.status.ExitCode = code
		c.status.FinishedAt = r.now()
	}
}

// FailOn makes op ("pull", "create", "start", "stop", "remove", "list") fail
// with err for id ("*" = any). A nil err clears the failure.
func (r *Runtime) FailOn(op, id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := op + " " + id
	if err == nil {
		delete(r.failures, key)
		return
	}
	r.failures[key] = err
}

// Calls returns and clears the recorded mutating calls.
func (r *Runtime) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	return c
}

func (r *Runtime) record(op, id string) error {
	r.calls = append(r.calls, op+" "+id)
	return r.failure(op, id)
}

func (r *Runtime) failure(op, id string) error {
	if err, ok := r.failures[op+" "+id]; ok {
		return err
	}
	return r.failures[op+" *"]
}

func (r *Runtime) EnsureImage(ctx context.Context, ref string) error {
	r.mu.Lock()
	if r.images[ref] {
		r.mu.Unlock()
		return nil
	}
	if err := r.record("pull", ref); err != nil {
		r.mu.Unlock()
		return err
	}
	hold := r.held[ref]
	r.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[ref] = true
	return nil
}

func (r *Runtime) Create(_ context.Context, spec runtime.ContainerSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record("create", spec.ID); err != nil {
		return err
	}
	if _, ok := r.containers[spec.ID]; ok {
		return fmt.Errorf("create %s: %w", spec.ID, runtime.ErrAlreadyExists)
	}
	if !r.images[spec.Image] {
		return fmt.Errorf("create %s: image %s not present", spec.ID, spec.Image)
	}
	r.containers[spec.ID] = &container{
		spec: spec,
		status: runtime.ContainerStatus{
			ID: spec.ID, Image: spec.Image, Labels: maps.Clone(spec.Labels), State: runtime.StateCreated,
		},
	}
	return nil
}

func (r *Runtime) Start(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("start %s: %w", id, runtime.ErrNotFound)
	}
	if c.status.State == runtime.StateRunning {
		return nil
	}
	if err := r.record("start", id); err != nil {
		return err
	}
	c.status.State = runtime.StateRunning
	c.status.Starts++
	c.status.StartedAt = r.now()
	c.status.ExitCode = 0
	c.status.FinishedAt = time.Time{}
	c.status.StopRequested = false
	c.status.IP = fmt.Sprintf("10.0.0.%d", len(r.containers))
	if r.crashing[c.spec.Image] {
		c.status.State, c.status.ExitCode, c.status.FinishedAt = runtime.StateExited, 1, r.now()
	}
	return nil
}

func (r *Runtime) Stop(_ context.Context, id string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok || c.status.State != runtime.StateRunning {
		return nil
	}
	if err := r.record("stop", id); err != nil {
		return err
	}
	c.status.State = runtime.StateExited
	c.status.ExitCode = 143 // 128 + SIGTERM
	c.status.FinishedAt = r.now()
	c.status.StopRequested = true
	return nil
}

func (r *Runtime) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[id]; !ok {
		return nil
	}
	if err := r.record("remove", id); err != nil {
		return err
	}
	delete(r.containers, id)
	return nil
}

func (r *Runtime) Status(_ context.Context, id string) (runtime.ContainerStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return runtime.ContainerStatus{}, fmt.Errorf("status %s: %w", id, runtime.ErrNotFound)
	}
	return cloneStatus(c.status), nil
}

func (r *Runtime) List(_ context.Context) ([]runtime.ContainerStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failure("list", "*"); err != nil {
		return nil, err
	}
	out := make([]runtime.ContainerStatus, 0, len(r.containers))
	for _, id := range slices.Sorted(maps.Keys(r.containers)) {
		out = append(out, cloneStatus(r.containers[id].status))
	}
	return out, nil
}

// Logs returns the lines set with SetLogs (honouring Tail). Follow is
// ignored: it returns once they are delivered.
func (r *Runtime) Logs(_ context.Context, id string, opts runtime.LogOptions, fn func(runtime.LogEntry) error) error {
	r.mu.Lock()
	if _, ok := r.containers[id]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("logs %s: %w", id, runtime.ErrNotFound)
	}
	entries := r.logs[id]
	r.mu.Unlock()
	if opts.Tail >= 0 && opts.Tail < len(entries) {
		entries = entries[len(entries)-opts.Tail:]
	}
	for _, e := range entries {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func cloneStatus(s runtime.ContainerStatus) runtime.ContainerStatus {
	s.Labels = maps.Clone(s.Labels)
	return s
}
