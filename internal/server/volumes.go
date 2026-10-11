package server

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/store"
)

// A Volume is referenced by at most one Workload (ADR 0016, section 1.1).
// The reference is a VolumeClaim record named after the Volume, whose spec
// is the owning workload's name. The store's Create fails with ErrExists
// when the record is already there, so two concurrent applies cannot both
// claim one Volume. A claim with an empty spec marks a Volume being deleted.

// volumeNames returns the Volume names a workload spec references, in order.
func volumeNames(s *v1alpha1.WorkloadSpec) []string {
	var out []string
	for _, m := range s.GetVolumes() {
		out = append(out, m.GetName())
	}
	return out
}

// claimOwner returns the workload that holds the claim on volume, or "" if
// none holds it or the volume is being deleted.
func (s *Server) claimOwner(ctx context.Context, volume string) (string, bool, error) {
	rec, err := s.store.Get(ctx, v1alpha1.KindVolumeClaim, volume)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(rec.Spec), true, nil
}

// claimVolumes takes the claims of volumes for workload. It returns the
// volumes it claimed, so the caller can release them if the apply fails.
// Volumes already claimed by workload are not claimed again.
func (s *Server) claimVolumes(ctx context.Context, workload string, volumes []string) ([]string, error) {
	var claimed []string
	for _, v := range volumes {
		_, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindVolumeClaim, Name: v, Spec: []byte(workload)})
		switch {
		case err == nil:
			claimed = append(claimed, v)
		case errors.Is(err, store.ErrExists):
			owner, held, gerr := s.claimOwner(ctx, v)
			if gerr != nil {
				return claimed, gerr
			}
			if !held {
				// Released between our Create and Get: try once more.
				return claimed, status.Errorf(codes.Aborted, "volume %q changed, try again", v)
			}
			if owner != workload {
				return claimed, conflictError(v, owner)
			}
		default:
			return claimed, err
		}
	}
	return claimed, nil
}

func conflictError(volume, owner string) error {
	if owner == "" {
		return status.Errorf(codes.InvalidArgument, "volume %q is being deleted", volume)
	}
	return status.Errorf(codes.InvalidArgument, "volume %q is already used by workload %q (a Volume has one workload)", volume, owner)
}

// releaseVolumes drops the claims of volumes that workload holds. Claims held
// by someone else (or by no one) are left alone.
func (s *Server) releaseVolumes(ctx context.Context, workload string, volumes []string) error {
	var errs []error
	for _, v := range volumes {
		owner, held, err := s.claimOwner(ctx, v)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !held || owner != workload {
			continue
		}
		if err := s.store.Delete(ctx, v1alpha1.KindVolumeClaim, v, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// storedVolumeNames returns the Volume names a stored Workload references,
// or nil if the Workload does not exist.
func (s *Server) storedVolumeNames(ctx context.Context, workload string) ([]string, error) {
	rec, err := s.store.Get(ctx, v1alpha1.KindWorkload, workload)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w, err := objects.Workload(rec)
	if err != nil {
		return nil, err
	}
	return volumeNames(w.Spec), nil
}

// deleteVolume deletes a Volume unless a Workload holds it. The marker claim
// is taken first, so a Workload cannot claim the Volume while it is deleted.
func (c *controlService) deleteVolume(ctx context.Context, name string) error {
	if _, err := c.s.store.Get(ctx, v1alpha1.KindVolume, name); err != nil {
		return apiError(err)
	}
	_, err := c.s.store.Create(ctx, store.Record{Kind: v1alpha1.KindVolumeClaim, Name: name, Spec: []byte{}})
	if errors.Is(err, store.ErrExists) {
		owner, _, gerr := c.s.claimOwner(ctx, name)
		if gerr != nil {
			return apiError(gerr)
		}
		if owner == "" {
			return status.Errorf(codes.FailedPrecondition, "volume %q is already being deleted", name)
		}
		return status.Errorf(codes.FailedPrecondition, "volume %q is used by workload %q; remove the workload first", name, owner)
	}
	if err != nil {
		return apiError(err)
	}
	defer func() {
		if err := c.s.store.Delete(context.WithoutCancel(ctx), v1alpha1.KindVolumeClaim, name, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			c.s.log.Warn("could not remove volume delete marker", "volume", name, "err", err)
		}
	}()
	return apiError(c.s.store.Delete(ctx, v1alpha1.KindVolume, name, 0))
}

// checkVolumeResize refuses a size change of a Volume that is bound to a node.
// Online resize is not in v1; growing or shrinking needs an offline step.
func checkVolumeResize(cur *v1alpha1.Volume, want *v1alpha1.VolumeSpec) error {
	if cur.GetStatus().GetNode() == "" || cur.GetSpec().GetSize() == want.GetSize() {
		return nil
	}
	return status.Errorf(codes.InvalidArgument, "volume %q is bound to node %q: its size cannot change in v1 (ADR 0016, section 1.2)",
		cur.GetMetadata().GetName(), cur.GetStatus().GetNode())
}

// missingVolumes returns the sorted names in volumes that exist neither in
// the store nor in batch (the Volumes of the same apply).
func (s *Server) missingVolumes(ctx context.Context, volumes []string, batch map[string]bool) ([]string, error) {
	var missing []string
	for _, v := range volumes {
		if batch[v] {
			continue
		}
		_, err := s.store.Get(ctx, v1alpha1.KindVolume, v)
		switch {
		case errors.Is(err, store.ErrNotFound):
			missing = append(missing, v)
		case err != nil:
			return nil, err
		}
	}
	slices.Sort(missing)
	return missing, nil
}
