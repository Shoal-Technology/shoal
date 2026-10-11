package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
)

// Rollout phases.
const (
	RolloutProgressing = "Progressing"
	RolloutComplete    = "Complete"
	RolloutRolledBack  = "RolledBack"
	RolloutFailed      = "Failed"
)

// crashLoopRestarts restarts of a replica on its new template fail the
// rollout without waiting for the deadline.
const crashLoopRestarts = 3

// plan is what one pass does with a workload's template.
type plan struct {
	target    *v1alpha1.WorkloadSpec // template replicas should run
	targetGen int64                  // workload generation it came from
	rollout   *v1alpha1.RolloutStatus
}

// available reports whether a replica is scheduled, ready, and its status
// describes the template it is supposed to run (not a previous one).
func available(r *replica) bool {
	st := r.obj.Status
	return r.obj.Spec.GetNode() != "" && st.GetReady() &&
		st.GetTemplateGeneration() == r.obj.Spec.GetTemplateGeneration()
}

func updated(r *replica, target *v1alpha1.WorkloadSpec) bool {
	return proto.Equal(r.obj.Spec.GetTemplate(), target)
}

// planRollout decides the workload's target template and rollout status.
//
// Normally the target is the spec's template. A rollout that fails (an
// updated replica isn't ready within the progress deadline, or crash-loops)
// records the generation as failed; while the spec stays at that
// generation, the target is the last stable template instead, i.e. the
// workload is rolled back. The spec itself is never changed: applying a
// new spec starts a new rollout.
func (c *Controller) planRollout(s *state, w *v1alpha1.Workload, now time.Time) plan {
	gen := w.Metadata.GetGeneration()
	desired := objects.Template(w.Spec)
	ro := proto.CloneOf(w.Status.GetRollout())
	if ro == nil {
		ro = &v1alpha1.RolloutStatus{}
	}
	stable := ro.GetStableTemplate()
	reps := s.replicasOf(w.Metadata.GetName())

	// Only a real rollout (from a known-good template to a different one)
	// can fail and be rolled back; a first deployment can only fail.
	rollingOut := !proto.Equal(stable, desired)
	if rollingOut && ro.FailedGeneration != gen {
		deadline := time.Duration(w.Spec.GetUpdate().GetProgressDeadlineSeconds()) * time.Second
		if why := failedReplica(reps, desired, deadline, now); why != "" {
			ro.FailedGeneration = gen
			if stable != nil {
				ro.Message = fmt.Sprintf("rollout of generation %d failed (%s); rolled back to generation %d", gen, why, ro.StableGeneration)
				c.log.Warn("rollout failed, rolling back", "workload", w.Metadata.GetName(), "generation", gen,
					"to", ro.StableGeneration, "reason", why)
			} else {
				ro.Message = fmt.Sprintf("generation %d is not becoming ready (%s); no earlier version to roll back to", gen, why)
				c.log.Warn("first rollout failing", "workload", w.Metadata.GetName(), "generation", gen, "reason", why)
			}
		}
	}

	p := plan{target: desired, targetGen: gen, rollout: ro}
	rolledBack := ro.FailedGeneration == gen && stable != nil && rollingOut
	if rolledBack {
		p.target, p.targetGen = stable, ro.StableGeneration
	}

	want := int(w.Spec.GetReplicas())
	done := len(reps) == want
	for _, r := range reps {
		if !updated(r, p.target) || !available(r) {
			done = false
		}
	}
	switch {
	case rolledBack:
		ro.Phase = RolloutRolledBack
	case done:
		// Everything runs the spec's template: it becomes the new stable one.
		ro.Phase, ro.Message = RolloutComplete, ""
		ro.StableTemplate, ro.StableGeneration, ro.FailedGeneration = desired, gen, 0
	case ro.FailedGeneration == gen:
		ro.Phase = RolloutFailed
	default:
		ro.Phase = RolloutProgressing
		ro.Message = fmt.Sprintf("%d of %d replicas updated and ready", countUpdatedAvailable(reps, p.target), want)
	}
	return p
}

// failedReplica returns why a replica on the desired template fails the
// rollout, or "" if none does.
func failedReplica(reps []*replica, desired *v1alpha1.WorkloadSpec, deadline time.Duration, now time.Time) string {
	for _, r := range reps {
		if !updated(r, desired) || available(r) {
			continue
		}
		st := r.obj.Status
		onTemplate := st.GetTemplateGeneration() == r.obj.Spec.GetTemplateGeneration()
		if onTemplate && st.GetRestarts() >= crashLoopRestarts {
			return fmt.Sprintf("replica %s restarted %d times", r.rec.Name, st.GetRestarts())
		}
		if since := r.obj.Spec.GetTemplateUpdatedAt(); since != nil && now.Sub(since.AsTime()) > deadline {
			why := fmt.Sprintf("replica %s not ready after %s", r.rec.Name, deadline)
			if onTemplate && st.GetMessage() != "" {
				why += ": " + st.GetMessage()
			}
			return why
		}
	}
	return ""
}

func countUpdatedAvailable(reps []*replica, target *v1alpha1.WorkloadSpec) int {
	n := 0
	for _, r := range reps {
		if updated(r, target) && available(r) {
			n++
		}
	}
	return n
}

// rollReplicas moves replicas to the plan's target template. Replicas that
// are unavailable anyway are updated at once; available ones only while
// fewer than maxUnavailable replicas are unavailable, so the next batch
// waits until updated replicas pass their health checks.
func (c *Controller) rollReplicas(ctx context.Context, s *state, w *v1alpha1.Workload, p plan, now time.Time) []error {
	reps := s.replicasOf(w.Metadata.GetName())
	var outdatedUp, outdatedDown []*replica
	availableCount := 0
	for _, r := range reps {
		if available(r) {
			availableCount++
		}
		if updated(r, p.target) {
			continue
		}
		if available(r) {
			outdatedUp = append(outdatedUp, r)
		} else {
			outdatedDown = append(outdatedDown, r)
		}
	}
	unavailable := int(w.Spec.GetReplicas()) - availableCount
	budget := max(int(w.Spec.GetUpdate().GetMaxUnavailable())-unavailable, 0)
	slices.SortFunc(outdatedUp, func(a, b *replica) int { return cmp.Compare(a.rec.Name, b.rec.Name) })
	toUpdate := append(outdatedDown, outdatedUp[:min(budget, len(outdatedUp))]...)

	var errs []error
	for _, r := range toUpdate {
		spec := proto.CloneOf(r.obj.Spec)
		spec.Template, spec.TemplateGeneration, spec.TemplateUpdatedAt = p.target, p.targetGen, timestamppb.New(now)
		if err := c.updateReplicaSpec(ctx, r, spec); err != nil {
			errs = append(errs, err)
			continue
		}
		c.log.Info("replica updated", "replica", r.rec.Name, "workload", w.Metadata.GetName(), "generation", p.targetGen)
	}
	return errs
}
