package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// digestRef matches an OCI digest such as sha256:<hex>. The digest goes into
// an image reference, so anything else is refused.
var digestRef = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]{32,}$`)

// rollOutNewest sets the Workload named by the newest completed Build of a
// config to the digest that Build pushed (OB-45). Only the newest completed
// Build counts, so an older run that finishes late never moves the Workload
// back. The Workload's spec changes, so the usual rollout and rollback apply
// (ADR 0010). The Build records the Workload once it is set, so later passes do
// nothing more. bs is sorted oldest first.
func (c *Controller) rollOutNewest(ctx context.Context, bs []*queuedBuild) error {
	var newest *queuedBuild
	for _, b := range bs {
		if b.phase() == resource.BuildComplete {
			newest = b
		}
	}
	if newest == nil {
		return nil
	}
	src := newest.obj.GetSpec().GetSource()
	target := src.GetOnSuccess().GetUpdateWorkload()
	st := newest.obj.GetStatus()
	if target == "" || st.GetWorkloadUpdated() == target {
		return nil
	}
	done := proto.CloneOf(st)
	if !digestRef.MatchString(st.GetDigest()) || src.GetOutput().GetRepository() == "" {
		done.Message = "the build has no digest to roll out"
		return c.writeBuildStatus(ctx, newest, done)
	}
	image := runtime.RegistryAlias + "/" + src.GetOutput().GetRepository() + "@" + st.GetDigest()
	err := c.setWorkloadImage(ctx, target, image)
	switch {
	case errors.Is(err, store.ErrNotFound):
		done.Message = fmt.Sprintf("workload %q not found: its image was not set", target)
	case err != nil:
		return err
	default:
		done.WorkloadUpdated, done.Message = target, ""
	}
	return c.writeBuildStatus(ctx, newest, done)
}

// setWorkloadImage replaces the image of a Workload's spec, through the store's
// compare-and-swap, so the generation moves and the rollout starts. An image
// that is already set is left alone.
func (c *Controller) setWorkloadImage(ctx context.Context, name, image string) error {
	rec, err := c.st.Get(ctx, v1alpha1.KindWorkload, name)
	if err != nil {
		return err
	}
	w, err := objects.Workload(rec)
	if err != nil {
		return err
	}
	if w.GetSpec().GetImage() == image {
		return nil
	}
	spec := proto.CloneOf(w.GetSpec())
	spec.Image = image
	enc, err := objects.Encode(spec)
	if err != nil {
		return err
	}
	if _, err := c.st.Update(ctx, v1alpha1.KindWorkload, name, enc, rec.Generation); err != nil {
		return fmt.Errorf("update workload %s: %w", name, err)
	}
	c.log.Info("workload image set from build", "workload", name, "image", image)
	return nil
}
