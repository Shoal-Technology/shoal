package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/volume"
)

// Config tunes the reconciler. Zero values get defaults.
type Config struct {
	StopTimeout    time.Duration // grace period before SIGKILL, default 10s
	PullTimeout    time.Duration // default 10m
	PullWait       time.Duration // pass waits this long for a new pull before moving on, default 2s
	OpTimeout      time.Duration // other runtime calls, default 30s
	BackoffInitial time.Duration // first restart delay, default 1s
	BackoffMax     time.Duration // default 5m
	BackoffReset   time.Duration // running this long clears the backoff, default 10m
	Now            func() time.Time
	// FilesDir holds the files of Secret and Config mounts, one directory
	// per replica. Required for assignments with files.
	FilesDir string
	// Volumes creates and retires the node's volumes (ADR 0016). Nil means
	// the node has no volume support: a replica with a Volume then waits.
	Volumes volume.Manager
}

func (c *Config) setDefaults() {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.StopTimeout, 10*time.Second)
	def(&c.PullTimeout, 10*time.Minute)
	def(&c.PullWait, 2*time.Second)
	def(&c.OpTimeout, 30*time.Second)
	def(&c.BackoffInitial, time.Second)
	def(&c.BackoffMax, 5*time.Minute)
	def(&c.BackoffReset, 10*time.Minute)
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Reconciler converges the runtime toward a set of assignments.
//
// It is level-triggered: each Reconcile call compares the full desired set
// with what the runtime reports and acts on the difference, so it is safe to
// call repeatedly and after any partial failure. The only in-memory state is
// restart backoff and image pulls in progress, both of which may be lost on
// agent restart.
//
// Image pulls run in the background: a replica whose image is still being
// pulled stays Pending and is created on a later pass.
//
// A Reconciler is not safe for concurrent use. Call Close when done.
type Reconciler struct {
	rt      runtime.Runtime
	log     *slog.Logger
	cfg     Config
	backoff map[string]*backoff
	puller  *imagePuller
	prober  *prober
	probes  map[string]probeTarget // collected during a pass, then synced
	// knownVolumes is the set of volumes the server assigned to this node,
	// from SetVolumes. Nil until it is set: then no volume is retired.
	knownVolumes map[string]bool
}

// SetVolumes gives the reconciler the names of the volumes that should be on
// this node. A volume on disk that is not in the set is retired on the next
// pass, after the containers that use it are gone: its data is retained or
// deleted according to its retention (ADR 0016, section 1.6).
func (r *Reconciler) SetVolumes(names []string) {
	r.knownVolumes = make(map[string]bool, len(names))
	for _, n := range names {
		r.knownVolumes[n] = true
	}
}

// retireVolumes retires the volumes on disk that are not assigned. It runs
// only after the unassigned containers were removed without error, so no
// container still uses a volume it retires.
func (r *Reconciler) retireVolumes(ctx context.Context) []error {
	onDisk, err := r.cfg.Volumes.Known()
	if err != nil {
		return []error{fmt.Errorf("list volumes: %w", err)}
	}
	var errs []error
	for _, name := range onDisk {
		if r.knownVolumes[name] {
			continue
		}
		r.log.Info("retiring volume that is no longer assigned", "volume", name)
		if err := r.cfg.Volumes.Retire(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("retire volume %s: %w", name, err))
		}
	}
	return errs
}

// prepare readies what a replica needs before its container is reconciled:
// its files and its volumes.
func (r *Reconciler) prepare(ctx context.Context, a *Assignment) error {
	if err := r.syncFiles(*a); err != nil {
		return err
	}
	return r.attachVolumes(ctx, a)
}

// attachVolumes makes the replica's volumes exist and adds their directories
// to the container's mounts.
func (r *Reconciler) attachVolumes(ctx context.Context, a *Assignment) error {
	for _, v := range a.Volumes {
		if r.cfg.Volumes == nil {
			return fmt.Errorf("volume %s: this node has no volume support", v.Name)
		}
		dir, err := r.cfg.Volumes.Ensure(ctx, v.Spec)
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		a.Spec.Mounts = append(a.Spec.Mounts, runtime.Mount{Source: dir, Destination: v.Destination, ReadWrite: v.ReadWrite})
	}
	return nil
}

type backoff struct {
	delay time.Duration
	next  time.Time
}

// NewReconciler returns a Reconciler for rt.
func NewReconciler(rt runtime.Runtime, log *slog.Logger, cfg Config) *Reconciler {
	cfg.setDefaults()
	return &Reconciler{
		rt:      rt,
		log:     log,
		cfg:     cfg,
		backoff: map[string]*backoff{},
		puller:  newImagePuller(rt, cfg.PullTimeout, cfg.PullWait),
		prober:  newProber(rt),
	}
}

// Close cancels image pulls and health probes in progress and waits for
// them to stop.
func (r *Reconciler) Close() {
	r.puller.close()
	r.prober.close()
}

// Reconcile performs one pass. It returns a status for every desired replica
// (sorted by ID) and an error joining all per-replica failures; a failure of
// one replica never prevents work on the others.
func (r *Reconciler) Reconcile(ctx context.Context, desired []Assignment) ([]ReplicaStatus, error) {
	lctx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	actualList, err := r.rt.List(lctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	actual := make(map[string]runtime.ContainerStatus, len(actualList))
	for _, c := range actualList {
		actual[c.ID] = c
	}
	want := make(map[string]bool, len(desired))
	for _, a := range desired {
		want[a.ReplicaID] = true
	}

	var errs []error
	// Containers that are no longer assigned are stopped and removed.
	for _, id := range slices.Sorted(maps.Keys(actual)) {
		if want[id] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.log.Info("removing unassigned container", "replica", id)
		if err := r.removeContainer(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("replica %s: %w", id, err))
		}
	}
	for id := range r.backoff {
		if !want[id] {
			delete(r.backoff, id)
		}
	}

	r.removeStaleFiles(want)
	if len(errs) == 0 && r.knownVolumes != nil && r.cfg.Volumes != nil {
		errs = append(errs, r.retireVolumes(ctx)...)
	}

	sorted := slices.SortedFunc(slices.Values(desired), func(a, b Assignment) int {
		return cmp.Compare(a.ReplicaID, b.ReplicaID)
	})
	statuses := make([]ReplicaStatus, 0, len(sorted))
	r.probes = map[string]probeTarget{}
	for _, a := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, exists := actual[a.ReplicaID]
		a.Spec.Mounts = r.mounts(a)
		var st ReplicaStatus
		var err error
		if a.Problem != "" {
			st = r.reportProblem(a, c, exists)
		} else if err = r.prepare(ctx, &a); err == nil {
			st, err = r.reconcileOne(ctx, a, c, exists)
		} else {
			st = ReplicaStatus{ReplicaID: a.ReplicaID, Phase: PhasePending}
			if exists {
				fill(&st, c)
			}
		}
		if err != nil {
			err = fmt.Errorf("replica %s: %w", a.ReplicaID, err)
			errs = append(errs, err)
			st.Message = err.Error()
		}
		// The status describes the container that runs. While an outdated
		// container keeps running (the new image is being pulled), report
		// the generation it was created for, so the server doesn't take the
		// old container's readiness for the new template's.
		st.TemplateGeneration = a.TemplateGeneration
		if c, ok := actual[a.ReplicaID]; ok && st.outdated {
			st.TemplateGeneration, _ = strconv.ParseInt(c.Labels[LabelTemplateGen], 10, 64)
		}
		r.setReadiness(a, &st)
		statuses = append(statuses, st)
	}
	// Probe exactly the running replicas that have a health check.
	r.prober.sync(r.probes)
	return statuses, errors.Join(errs...)
}

// reportProblem reports an assignment the server couldn't resolve (e.g. a
// missing Secret). The container, if any, is left as it is: a deleted
// Secret must not take running replicas down.
func (r *Reconciler) reportProblem(a Assignment, c runtime.ContainerStatus, exists bool) ReplicaStatus {
	st := ReplicaStatus{ReplicaID: a.ReplicaID, Phase: PhasePending}
	if exists {
		fill(&st, c)
	}
	st.Message = a.Problem
	return st
}

// mounts returns the bind mounts for an assignment's files.
func (r *Reconciler) mounts(a Assignment) []runtime.Mount {
	var out []runtime.Mount
	for i, f := range a.Files {
		out = append(out, runtime.Mount{Source: r.filesDir(a.ReplicaID, i), Destination: f.Path})
	}
	return out
}

func (r *Reconciler) filesDir(replica string, i int) string {
	return filepath.Join(r.cfg.FilesDir, replica, strconv.Itoa(i))
}

// syncFiles writes the assignment's files, rewriting only what changed and
// removing files whose keys are gone. Mounted directories show the new
// content to running containers without a restart.
func (r *Reconciler) syncFiles(a Assignment) error {
	if len(a.Files) == 0 {
		return nil
	}
	if r.cfg.FilesDir == "" {
		return errors.New("assignment has files but the agent has no files directory")
	}
	for i, f := range a.Files {
		dir := r.filesDir(a.ReplicaID, i)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create files directory: %w", err)
		}
		for name, content := range f.Files {
			p := filepath.Join(dir, name)
			if cur, err := os.ReadFile(p); err == nil && string(cur) == content {
				continue
			}
			// Readable inside the container whatever its user; on the host
			// the state directory is root-only.
			if err := security.WriteFileAtomic(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("list files: %w", err)
		}
		for _, e := range entries {
			if _, ok := f.Files[e.Name()]; !ok {
				if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
					return fmt.Errorf("remove stale file: %w", err)
				}
			}
		}
	}
	return nil
}

// removeStaleFiles deletes the files of replicas no longer assigned.
func (r *Reconciler) removeStaleFiles(want map[string]bool) {
	if r.cfg.FilesDir == "" {
		return
	}
	entries, err := os.ReadDir(r.cfg.FilesDir)
	if err != nil {
		return // not created yet
	}
	for _, e := range entries {
		if !want[e.Name()] {
			if err := os.RemoveAll(filepath.Join(r.cfg.FilesDir, e.Name())); err != nil {
				r.log.Warn("remove files of an unassigned replica", "replica", e.Name(), "err", err)
			}
		}
	}
}

// setReadiness marks a running replica ready: at once without a health
// check, otherwise from its probe, which it registers for this pass.
func (r *Reconciler) setReadiness(a Assignment, st *ReplicaStatus) {
	if st.Phase != PhaseRunning {
		return
	}
	if a.Health == nil {
		st.Ready = true
		return
	}
	if st.IP == "" {
		st.Message = "no IP to run the health check against"
		return
	}
	t := probeTarget{id: a.ReplicaID, ip: st.IP, startedAt: st.StartedAt, check: *a.Health}
	r.probes[a.ReplicaID] = t
	h := r.prober.result(a.ReplicaID, t)
	st.Ready = h.ready
	if !h.ready && st.Message == "" {
		st.Message = h.message
	}
}

// checkHealth restarts a running replica whose probe has failed
// failureThreshold times in a row.
func (r *Reconciler) checkHealth(ctx context.Context, a Assignment, c runtime.ContainerStatus, st ReplicaStatus) (ReplicaStatus, error) {
	if a.Health == nil || c.IP == "" {
		return st, nil
	}
	id := a.ReplicaID
	h := r.prober.result(id, probeTarget{id: id, ip: c.IP, startedAt: c.StartedAt, check: *a.Health})
	if !h.unhealthy {
		return st, nil
	}
	r.log.Warn("health check failing, restarting container", "replica", id, "reason", h.message)
	r.prober.forget(id)
	if err := r.stop(ctx, id); err != nil {
		return st, err
	}
	restarted, err := r.startAndReport(ctx, id)
	if restarted.Message == "" {
		restarted.Message = "restarted: " + h.message
	}
	return restarted, err
}

func (r *Reconciler) reconcileOne(ctx context.Context, a Assignment, c runtime.ContainerStatus, exists bool) (ReplicaStatus, error) {
	id := a.ReplicaID
	st := ReplicaStatus{ReplicaID: id, Phase: PhasePending}

	if exists && c.Labels[LabelSpecHash] != a.specHash() {
		// Keep the old container running until the new image is available.
		if a.Desired == DesiredRunning {
			st.outdated = true // set on every early return below
			if msg, waiting := r.inBackoff(id); waiting {
				fill(&st, c)
				st.Message = msg
				return st, nil
			}
			ready, err := r.puller.ensure(ctx, a.Spec.Image)
			if err != nil {
				r.recordAttempt(id)
				fill(&st, c)
				return st, err
			}
			if !ready {
				fill(&st, c)
				st.Message = "pulling image " + a.Spec.Image
				return st, nil
			}
			st.outdated = false
		}
		r.log.Info("spec changed, recreating container", "replica", id)
		if err := r.removeContainer(ctx, id); err != nil {
			return st, err
		}
		exists = false
	}

	if a.Desired == DesiredStopped {
		if exists && c.State == runtime.StateRunning {
			r.log.Info("stopping container", "replica", id)
			if err := r.stop(ctx, id); err != nil {
				fill(&st, c)
				return st, err
			}
			if c, err := r.status(ctx, id); err == nil {
				fill(&st, c)
			}
		} else if exists {
			fill(&st, c)
		}
		st.Phase = PhaseStopped
		return st, nil
	}

	if !exists {
		if msg, waiting := r.inBackoff(id); waiting {
			st.Message = msg
			return st, nil
		}
		created, err := r.create(ctx, a)
		if err != nil {
			r.recordAttempt(id)
			return st, err
		}
		if !created {
			st.Message = "pulling image " + a.Spec.Image
			return st, nil
		}
		return r.startAndReport(ctx, id)
	}

	switch c.State {
	case runtime.StateRunning:
		if r.cfg.Now().Sub(c.StartedAt) >= r.cfg.BackoffReset {
			delete(r.backoff, id)
		}
		fill(&st, c)
		return r.checkHealth(ctx, a, c, st)

	case runtime.StateCreated:
		if msg, waiting := r.inBackoff(id); waiting {
			st.Message = msg
			return st, nil
		}
		return r.startAndReport(ctx, id)

	case runtime.StateExited:
		fill(&st, c)
		// Stopped on request (desired was Stopped): resume regardless of the
		// restart policy and without waiting for backoff.
		if c.StopRequested {
			r.log.Info("starting stopped container", "replica", id)
			return r.startAndReport(ctx, id)
		}
		if !shouldRestart(a.RestartPolicy, c.ExitCode) {
			if c.ExitCode == 0 {
				st.Phase = PhaseSucceeded
			} else {
				st.Phase = PhaseFailed
			}
			return st, nil
		}
		st.Phase = PhaseFailed
		if msg, waiting := r.inBackoff(id); waiting {
			st.Message = fmt.Sprintf("exited with code %d, %s", c.ExitCode, msg)
			return st, nil
		}
		r.log.Info("restarting container", "replica", id, "exitCode", c.ExitCode, "restarts", st.Restarts)
		return r.startAndReport(ctx, id)

	default:
		fill(&st, c)
		st.Message = fmt.Sprintf("container in state %q", c.State)
		return st, nil
	}
}

// startAndReport starts the container. Every start attempt counts toward
// backoff, so a crash-looping container is restarted with growing delays.
func (r *Reconciler) startAndReport(ctx context.Context, id string) (ReplicaStatus, error) {
	st := ReplicaStatus{ReplicaID: id, Phase: PhasePending}
	r.recordAttempt(id)
	octx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	err := r.rt.Start(octx, id)
	cancel()
	if err != nil {
		return st, err
	}
	c, err := r.status(ctx, id)
	if err != nil {
		return st, err
	}
	fill(&st, c)
	return st, nil
}

// create creates the container once its image is available. It returns
// false, without error, while the image is still being pulled.
func (r *Reconciler) create(ctx context.Context, a Assignment) (bool, error) {
	ready, err := r.puller.ensure(ctx, a.Spec.Image)
	if err != nil || !ready {
		return false, err
	}
	r.log.Info("creating container", "replica", a.ReplicaID, "image", a.Spec.Image)
	cctx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	defer cancel()
	if err := r.rt.Create(cctx, a.containerSpec()); err != nil && !errors.Is(err, runtime.ErrAlreadyExists) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) stop(ctx context.Context, id string) error {
	sctx, cancel := context.WithTimeout(ctx, r.cfg.StopTimeout+r.cfg.OpTimeout)
	defer cancel()
	return r.rt.Stop(sctx, id, r.cfg.StopTimeout)
}

// removeContainer stops gracefully, then removes. Remove force-kills, so a
// failed graceful stop is logged but does not block removal.
func (r *Reconciler) removeContainer(ctx context.Context, id string) error {
	if err := r.stop(ctx, id); err != nil {
		r.log.Warn("graceful stop failed, forcing removal", "replica", id, "err", err)
	}
	rctx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	defer cancel()
	return r.rt.Remove(rctx, id)
}

func (r *Reconciler) status(ctx context.Context, id string) (runtime.ContainerStatus, error) {
	sctx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	defer cancel()
	return r.rt.Status(sctx, id)
}

// inBackoff reports whether the next attempt for id must wait. The message
// names the current delay rather than a countdown, so it stays the same
// between attempts and status changes are not reported on every pass.
func (r *Reconciler) inBackoff(id string) (string, bool) {
	b, ok := r.backoff[id]
	if !ok || !r.cfg.Now().Before(b.next) {
		return "", false
	}
	return fmt.Sprintf("back-off %s before next attempt", b.delay), true
}

// recordAttempt registers a (re)start attempt and schedules the earliest
// time for the next one, doubling the delay each time.
func (r *Reconciler) recordAttempt(id string) {
	b, ok := r.backoff[id]
	if !ok {
		b = &backoff{delay: r.cfg.BackoffInitial}
		r.backoff[id] = b
	} else {
		b.delay = min(b.delay*2, r.cfg.BackoffMax)
	}
	b.next = r.cfg.Now().Add(b.delay)
}

func shouldRestart(p RestartPolicy, exitCode int) bool {
	switch p {
	case RestartNever:
		return false
	case RestartOnFailure:
		return exitCode != 0
	default:
		return true
	}
}

func fill(st *ReplicaStatus, c runtime.ContainerStatus) {
	switch c.State {
	case runtime.StateRunning:
		st.Phase = PhaseRunning
	case runtime.StateExited:
		st.Phase = PhaseFailed
		if c.ExitCode == 0 {
			st.Phase = PhaseSucceeded
		}
	}
	st.Restarts = max(c.Starts-1, 0)
	st.ExitCode = c.ExitCode
	st.IP = c.IP
	st.StartedAt = c.StartedAt
}
