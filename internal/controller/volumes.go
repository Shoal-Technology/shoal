package controller

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Volume states (ADR 0016, section 1).
const (
	volumePending  = "Pending"  // not yet bound to a node
	volumeBound    = "Bound"    // bound to a node that is Ready
	volumeNodeLost = "NodeLost" // bound to a node that is not Ready
)

// volumePlan is what a replica needs from the Volumes it mounts.
type volumePlan struct {
	node  string // node the bound Volumes are on; "" when none is bound yet
	bound string // a bound Volume's name, for messages
	fresh []*v1alpha1.Volume
	bytes int64 // total size of fresh
}

// planVolumes works out where a replica with template t may run because of its
// Volumes. Bound Volumes pin the replica to their node (ADR 0016, 1.4). It
// returns a non-empty reason if the Volumes can never be used together.
func (s *state) planVolumes(t *v1alpha1.WorkloadSpec) (volumePlan, string) {
	var p volumePlan
	for _, m := range t.GetVolumes() {
		v, ok := s.volumes[m.GetName()]
		if !ok {
			return p, fmt.Sprintf("unschedulable: volume %s not found", m.GetName())
		}
		if n := v.Status.GetNode(); n != "" {
			if p.node != "" && p.node != n {
				return p, fmt.Sprintf("unschedulable: volumes %s and %s are on different nodes (%s, %s)", p.bound, m.GetName(), p.node, n)
			}
			p.node, p.bound = n, m.GetName()
			continue
		}
		size, err := resource.VolumeBytes(v.Spec)
		if err != nil {
			return p, fmt.Sprintf("unschedulable: volume %s: %v", m.GetName(), err)
		}
		p.fresh = append(p.fresh, v)
		p.bytes += size
	}
	return p, ""
}

// bindVolumes sets the Volumes of a replica being bound to node. It runs
// before the replica's own binding is written: the Volume records the node
// first, and the replica follows. If the second write fails, the next pass
// binds the replica to the same node, because the Volume pins it there, so the
// two writes converge (ADR 0016, section 1.4).
func (c *Controller) bindVolumes(ctx context.Context, s *state, plan volumePlan, node, workload string) error {
	for _, v := range plan.fresh {
		st := &v1alpha1.VolumeStatus{Node: node, State: volumeBound, AttachedWorkload: workload}
		if err := c.setVolumeStatus(ctx, s, v, st); err != nil {
			return err
		}
	}
	return nil
}

// setVolumeStatus writes a Volume's status (the controller is its only writer).
func (c *Controller) setVolumeStatus(ctx context.Context, s *state, v *v1alpha1.Volume, st *v1alpha1.VolumeStatus) error {
	if proto.Equal(v.Status, st) {
		return nil
	}
	b, err := objects.Encode(st)
	if err != nil {
		return err
	}
	name := objects.KeyOf(v.GetMetadata())
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindVolume, name, b); err != nil {
		return fmt.Errorf("update status of volume %s: %w", name, err)
	}
	v.Status = st
	return nil
}

// syncVolumes derives each Volume's state, node loss and attached workload
// from the cluster, and writes the status when it changes. A Volume bound to a
// node that is not Ready is NodeLost, and it returns to Bound when the node
// comes back (ADR 0016, section 1.5).
func (c *Controller) syncVolumes(ctx context.Context, s *state, now time.Time) []error {
	users := map[string]string{}
	for name, w := range s.workloads {
		for _, m := range w.Spec.GetVolumes() {
			users[tenancy.Key(w.GetMetadata().GetProject(), m.GetName())] = name
		}
	}
	var errs []error
	for _, name := range sortedKeys(s.volumes) {
		v := s.volumes[name]
		want := &v1alpha1.VolumeStatus{Node: v.Status.GetNode(), State: volumePending, AttachedWorkload: users[name]}
		if node := want.Node; node != "" {
			want.State = volumeBound
			if lost, _ := c.nodeLost(s, node, now); lost {
				want.State = volumeNodeLost
				want.Message = fmt.Sprintf("node %s is not ready; its data stays on that node", node)
			}
			if n, ok := s.nodes[node]; ok {
				switch n.Status.GetVolumeBackend() {
				case "loop":
					want.Quota = "size"
				case "dir":
					want.Quota = "none"
				}
			}
		}
		if err := c.setVolumeStatus(ctx, s, v, want); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// hasVolumes reports whether a replica mounts any Volume.
func hasVolumes(r *replica) bool {
	return len(r.obj.Spec.GetTemplate().GetVolumes()) > 0
}

// holdVolumeReplica keeps a replica that mounts a Volume on its lost node
// instead of replacing it: a second writer on another node would corrupt the
// data (ADR 0016, section 1.5). The replica is reported Pending with the reason.
func (c *Controller) holdVolumeReplica(ctx context.Context, r *replica, node string) error {
	st := proto.CloneOf(r.obj.Status)
	st.Phase = v1alpha1.ReplicaPending
	st.Message = fmt.Sprintf("node %s is not ready: the replica stays with its volume until the node returns", node)
	return c.setReplicaStatus(ctx, r, st)
}
