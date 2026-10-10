package server

import (
	"context"
	"errors"
	"maps"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/store"
)

// labelKey is an optional DNS-style prefix, a slash, then a name of up to 63
// characters (the Kubernetes label key shape).
var labelKey = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// labelValue is up to 63 characters from the label value alphabet, possibly empty.
var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)

// SetNodeLabels changes a node's labels. The operator uses it through
// `shoal label node`. It is the only writer of Node labels.
func (c *controlService) SetNodeLabels(ctx context.Context, req *v1alpha1.SetNodeLabelsRequest) (*v1alpha1.SetNodeLabelsResponse, error) {
	for k, v := range req.GetSet() {
		if !labelKey.MatchString(k) {
			return nil, status.Errorf(codes.InvalidArgument, "label key %q is not valid", k)
		}
		if !labelValue.MatchString(v) {
			return nil, status.Errorf(codes.InvalidArgument, "value %q of label %s is not valid (at most 63 letters, digits, '-', '_' or '.')", v, k)
		}
	}
	for range casRetries {
		rec, err := c.s.store.Get(ctx, v1alpha1.KindNode, req.GetNode())
		if err != nil {
			return nil, apiError(err)
		}
		n, err := objects.Node(rec)
		if err != nil {
			return nil, err
		}
		labels := maps.Clone(n.Spec.GetLabels())
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, req.GetSet())
		for _, k := range req.GetRemove() {
			delete(labels, k)
		}
		n.Spec.Labels = labels
		b, err := objects.Encode(n.Spec)
		if err != nil {
			return nil, err
		}
		_, err = c.s.store.Update(ctx, v1alpha1.KindNode, req.GetNode(), b, rec.Generation)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		c.s.log.Info("node labels changed", "node", req.GetNode(), "set", len(req.GetSet()), "removed", len(req.GetRemove()))
		return &v1alpha1.SetNodeLabelsResponse{Labels: labels}, nil
	}
	return nil, status.Error(codes.Aborted, "node changed concurrently; try again")
}
