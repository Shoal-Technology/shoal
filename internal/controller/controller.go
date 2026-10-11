// Package controller turns Workloads into scheduled Replicas and rolls
// template changes out gradually, with automatic rollback.
//
// Reconcile is one level-triggered pass over the whole cluster state. It
// is idempotent: a pass over a converged state writes nothing. Each write is
// a single-object compare-and-swap; a conflict or failure only skips that
// object until the next pass, so partial failures never leave the state
// inconsistent beyond what the next pass repairs.
package controller

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/netip"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/store"
)

// Config configures the controller. Zero values get defaults.
type Config struct {
	Store store.Store
	Log   *slog.Logger
	Now   func() time.Time
	// EvictAfter is how long a NotReady node may go without a heartbeat
	// before its replicas are replaced on other nodes. Default 60s.
	EvictAfter time.Duration
	// Interval is the period of passes not triggered by changes. Default 5s.
	Interval time.Duration
	// ServiceCIDR holds Service virtual IPs. Default 10.96.0.0/16.
	ServiceCIDR netip.Prefix
}

// Controller reconciles workloads, replicas and nodes.
type Controller struct {
	cfg Config
	st  store.Store
	log *slog.Logger
}

// New returns a controller.
func New(cfg Config) *Controller {
	if cfg.EvictAfter <= 0 {
		cfg.EvictAfter = 60 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if !cfg.ServiceCIDR.IsValid() {
		cfg.ServiceCIDR = netip.MustParsePrefix("10.96.0.0/16")
	}
	return &Controller{cfg: cfg, st: cfg.Store, log: cfg.Log}
}

// Run reconciles on every relevant change (debounced) and every Interval,
// until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	events := c.st.Watch(ctx, v1alpha1.KindWorkload, v1alpha1.KindNode, v1alpha1.KindReplica, v1alpha1.KindService, v1alpha1.KindRoute, v1alpha1.KindSecret, v1alpha1.KindCertificate, v1alpha1.KindVolume)
	tick := time.NewTicker(c.cfg.Interval)
	defer tick.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("reconcile incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case _, ok := <-events:
			if !ok {
				return
			}
			// Let a burst of changes (e.g. a multi-object apply) settle into
			// one pass.
			settle := time.NewTimer(50 * time.Millisecond)
		drain:
			for {
				select {
				case <-events:
				case <-settle.C:
					break drain
				case <-ctx.Done():
					settle.Stop()
					return
				}
			}
		}
	}
}

type replica struct {
	rec store.Record
	obj *v1alpha1.Replica
}

// state is the cluster as one pass sees it, kept up to date with the pass's
// own writes.
type state struct {
	workloads map[string]*v1alpha1.Workload
	nodes     map[string]*v1alpha1.Node
	replicas  map[string]*replica
	services  map[string]*v1alpha1.Service
	volumes   map[string]*v1alpha1.Volume
	plans     map[string]plan // per workload, from syncWorkload
}

func (s *state) replicasOf(workload string) []*replica {
	var out []*replica
	for _, r := range s.replicas {
		if r.obj.Spec.GetWorkload() == workload {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *replica) int { return cmp.Compare(a.rec.Name, b.rec.Name) })
	return out
}

// Reconcile performs one pass and returns the errors it skipped over.
func (c *Controller) Reconcile(ctx context.Context) error {
	s, errs := c.load(ctx)
	if s == nil {
		return errors.Join(errs...)
	}
	now := c.cfg.Now()
	errs = append(errs, c.removeOrphansAndEvict(ctx, s, now)...)
	for _, name := range sortedKeys(s.workloads) {
		errs = append(errs, c.syncWorkload(ctx, s, s.workloads[name], now)...)
	}
	errs = append(errs, c.schedule(ctx, s)...)
	errs = append(errs, c.syncVolumes(ctx, s, now)...)
	errs = append(errs, c.syncServices(ctx, s)...)
	errs = append(errs, c.syncRouteStatus(ctx)...)
	for _, name := range sortedKeys(s.workloads) {
		if err := c.updateWorkloadStatus(ctx, s, s.workloads[name]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) load(ctx context.Context) (*state, []error) {
	s := &state{workloads: map[string]*v1alpha1.Workload{}, nodes: map[string]*v1alpha1.Node{}, replicas: map[string]*replica{}, services: map[string]*v1alpha1.Service{}, volumes: map[string]*v1alpha1.Volume{}, plans: map[string]plan{}}
	var errs []error
	wrecs, err := c.st.List(ctx, v1alpha1.KindWorkload)
	if err != nil {
		return nil, []error{err}
	}
	vrecs, err := c.st.List(ctx, v1alpha1.KindVolume)
	if err != nil {
		return nil, []error{err}
	}
	for _, r := range vrecs {
		v, err := objects.Volume(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.volumes[r.Name] = v
	}
	nrecs, err := c.st.List(ctx, v1alpha1.KindNode)
	if err != nil {
		return nil, []error{err}
	}
	rrecs, err := c.st.List(ctx, v1alpha1.KindReplica)
	if err != nil {
		return nil, []error{err}
	}
	srecs, err := c.st.List(ctx, v1alpha1.KindService)
	if err != nil {
		return nil, []error{err}
	}
	for _, r := range srecs {
		svc, err := objects.Service(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.services[r.Name] = svc
	}
	for _, r := range wrecs {
		w, err := objects.Workload(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.workloads[r.Name] = w
	}
	for _, r := range nrecs {
		n, err := objects.Node(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.nodes[r.Name] = n
	}
	for _, r := range rrecs {
		rp, err := objects.Replica(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.replicas[r.Name] = &replica{rec: r, obj: rp}
	}
	return s, errs
}

// nodeLost reports whether a bound replica's node is gone or has been
// silent for longer than EvictAfter. A Ready node is never lost: its stored
// heartbeat time may lag by up to the status persist interval.
func (c *Controller) nodeLost(s *state, name string, now time.Time) (bool, string) {
	n, ok := s.nodes[name]
	if !ok {
		return true, "node deleted"
	}
	if n.Status.GetPhase() == v1alpha1.NodeReady {
		return false, ""
	}
	last := n.Metadata.GetCreatedAt().AsTime()
	if hb := n.Status.GetLastHeartbeat(); hb != nil {
		last = hb.AsTime()
	}
	if now.Sub(last) < c.cfg.EvictAfter {
		return false, ""
	}
	return true, fmt.Sprintf("node not ready for %s", now.Sub(last).Round(time.Second))
}

func (c *Controller) removeOrphansAndEvict(ctx context.Context, s *state, now time.Time) []error {
	var errs []error
	for _, name := range sortedKeys(s.replicas) {
		r := s.replicas[name]
		reason := ""
		if _, ok := s.workloads[r.obj.Spec.GetWorkload()]; !ok {
			reason = "workload deleted"
		} else if node := r.obj.Spec.GetNode(); node != "" {
			if lost, why := c.nodeLost(s, node, now); lost {
				if s.workloads[r.obj.Spec.GetWorkload()].Spec.GetPartitionPolicy() == "Hold" {
					continue
				}
				// A replica with a volume is never replaced on another node
				// (ADR 0016, section 1.5). The Volume is marked NodeLost.
				if hasVolumes(r) {
					if err := c.holdVolumeReplica(ctx, r, node); err != nil {
						errs = append(errs, err)
					}
					continue
				}
				reason = why
			}
		}
		if reason == "" {
			continue
		}
		if err := c.deleteReplica(ctx, s, r, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (c *Controller) deleteReplica(ctx context.Context, s *state, r *replica, reason string) error {
	err := c.st.Delete(ctx, v1alpha1.KindReplica, r.rec.Name, r.rec.Generation)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete replica %s: %w", r.rec.Name, err)
	}
	delete(s.replicas, r.rec.Name)
	c.log.Info("replica removed", "replica", r.rec.Name, "workload", r.obj.Spec.GetWorkload(), "node", r.obj.Spec.GetNode(), "reason", reason)
	return nil
}

// syncWorkload brings the workload's replicas to the desired count and,
// with a rolling update, to the target template (see planRollout).
func (c *Controller) syncWorkload(ctx context.Context, s *state, w *v1alpha1.Workload, now time.Time) []error {
	name := w.Metadata.GetName()
	p := c.planRollout(s, w, now)
	s.plans[name] = p
	errs := c.rollReplicas(ctx, s, w, p, now)

	reps := s.replicasOf(name)
	want := int(w.Spec.GetReplicas())
	switch {
	case len(reps) < want:
		for range want - len(reps) {
			if err := c.createReplica(ctx, s, w, p, now); err != nil {
				errs = append(errs, err)
			}
		}
	case len(reps) > want:
		for _, r := range c.scaleDownOrder(s, reps, p.target)[:len(reps)-want] {
			if err := c.deleteReplica(ctx, s, r, "scaled down"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// scaleDownOrder sorts replicas so the cheapest to lose come first:
// unscheduled, then on a not-ready node, then unavailable, then outdated,
// then newest.
func (c *Controller) scaleDownOrder(s *state, reps []*replica, target *v1alpha1.WorkloadSpec) []*replica {
	rank := func(r *replica) int {
		node := r.obj.Spec.GetNode()
		switch {
		case node == "":
			return 0
		case s.nodes[node].GetStatus().GetPhase() != v1alpha1.NodeReady:
			return 1
		case !available(r):
			return 2
		case !updated(r, target):
			return 3
		}
		return 4
	}
	out := slices.Clone(reps)
	slices.SortStableFunc(out, func(a, b *replica) int {
		if d := cmp.Compare(rank(a), rank(b)); d != 0 {
			return d
		}
		if d := b.rec.CreatedAt.Compare(a.rec.CreatedAt); d != 0 {
			return d
		}
		return cmp.Compare(b.rec.Name, a.rec.Name)
	})
	return out
}

func (c *Controller) createReplica(ctx context.Context, s *state, w *v1alpha1.Workload, p plan, now time.Time) error {
	spec := &v1alpha1.ReplicaSpec{
		Workload: w.Metadata.GetName(), Template: p.target, TemplateGeneration: p.targetGen,
		TemplateUpdatedAt: timestamppb.New(now),
	}
	specBytes, err := objects.Encode(spec)
	if err != nil {
		return err
	}
	status := &v1alpha1.ReplicaStatus{Phase: v1alpha1.ReplicaPending, Message: "waiting to be scheduled"}
	statusBytes, err := objects.Encode(status)
	if err != nil {
		return err
	}
	for range 5 {
		name := w.Metadata.GetName() + "-" + randomSuffix()
		rec, err := c.st.Create(ctx, store.Record{Kind: v1alpha1.KindReplica, Name: name, Spec: specBytes, Status: statusBytes})
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create replica of %s: %w", w.Metadata.GetName(), err)
		}
		s.replicas[name] = &replica{rec: rec, obj: &v1alpha1.Replica{Metadata: objects.Meta(rec), Spec: spec, Status: status}}
		c.log.Info("replica created", "replica", name, "workload", w.Metadata.GetName())
		return nil
	}
	return fmt.Errorf("create replica of %s: no free name", w.Metadata.GetName())
}

func (c *Controller) updateReplicaSpec(ctx context.Context, r *replica, spec *v1alpha1.ReplicaSpec) error {
	b, err := objects.Encode(spec)
	if err != nil {
		return err
	}
	rec, err := c.st.Update(ctx, v1alpha1.KindReplica, r.rec.Name, b, r.rec.Generation)
	if err != nil {
		return fmt.Errorf("update replica %s: %w", r.rec.Name, err)
	}
	r.rec, r.obj.Spec = rec, spec
	return nil
}

func (c *Controller) setReplicaStatus(ctx context.Context, r *replica, st *v1alpha1.ReplicaStatus) error {
	if proto.Equal(r.obj.Status, st) {
		return nil
	}
	b, err := objects.Encode(st)
	if err != nil {
		return err
	}
	rec, err := c.st.UpdateStatus(ctx, v1alpha1.KindReplica, r.rec.Name, b)
	if err != nil {
		return fmt.Errorf("update status of replica %s: %w", r.rec.Name, err)
	}
	r.rec, r.obj.Status = rec, st
	return nil
}

func (c *Controller) updateWorkloadStatus(ctx context.Context, s *state, w *v1alpha1.Workload) error {
	p, ok := s.plans[w.Metadata.GetName()]
	if !ok {
		return nil // syncWorkload didn't run for it this pass
	}
	st := &v1alpha1.WorkloadStatus{ObservedGeneration: w.Metadata.GetGeneration(), Rollout: p.rollout}
	for _, r := range s.replicasOf(w.Metadata.GetName()) {
		st.Replicas++
		if available(r) {
			st.ReadyReplicas++
		}
		if updated(r, p.target) {
			st.UpdatedReplicas++
		}
	}
	if proto.Equal(w.Status, st) {
		return nil
	}
	b, err := objects.Encode(st)
	if err != nil {
		return err
	}
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindWorkload, w.Metadata.GetName(), b); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("update status of workload %s: %w", w.Metadata.GetName(), err)
	}
	w.Status = st
	return nil
}

const suffixAlphabet = "bcdfghjklmnpqrstvwxz2456789" // no vowels: no accidental words

func randomSuffix() string {
	b := make([]byte, 5)
	max := big.NewInt(int64(len(suffixAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand never fails on supported platforms
		}
		b[i] = suffixAlphabet[n.Int64()]
	}
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
