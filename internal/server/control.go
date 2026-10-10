package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/security"
	"example.com/shoal/internal/store"
)

const (
	defaultTokenTTL = 24 * time.Hour
	maxTokenTTL     = 30 * 24 * time.Hour
	// casRetries bounds read-modify-write loops under concurrent writers.
	casRetries = 5
)

type controlService struct {
	v1alpha1.UnimplementedControlServer
	s *Server
}

func (c *controlService) Apply(ctx context.Context, req *v1alpha1.ApplyRequest) (*v1alpha1.ApplyResponse, error) {
	if len(req.GetObjects()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no objects to apply")
	}
	// Validate everything first, so a bad object doesn't leave the batch
	// half-applied.
	type prepared struct {
		kind, name string
		spec       []byte
		msg        proto.Message
	}
	var (
		objs []prepared
		errs []error
	)
	for i, obj := range req.GetObjects() {
		kind, name, spec, err := resource.Prepare(obj)
		if err != nil {
			errs = append(errs, fmt.Errorf("object %d (%s %q): %w", i+1, kind, name, err))
			continue
		}
		b, err := encode(spec)
		if err != nil {
			return nil, apiError(err)
		}
		objs = append(objs, prepared{kind, name, b, spec})
	}
	// Routes must not share a host and path prefix with another Route, either
	// stored or earlier in this batch. The check is not atomic with the writes,
	// so two concurrent applies can still both pass; see ADR 0015 pass A.
	owners, err := c.s.routeOwners(ctx)
	if err != nil {
		return nil, apiError(err)
	}
	for _, o := range objs {
		if o.kind != v1alpha1.KindRoute {
			continue
		}
		key := resource.RouteKey(o.msg.(*v1alpha1.RouteSpec))
		if owner, ok := owners[key]; ok && owner != o.name {
			errs = append(errs, fmt.Errorf("route %q: host and pathPrefix are already used by route %q", o.name, owner))
		}
		owners[key] = o.name
	}
	if err := errors.Join(errs...); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	resp := &v1alpha1.ApplyResponse{}
	for _, o := range objs {
		action, err := c.s.applySpec(ctx, o.kind, o.name, o.spec)
		if err != nil {
			return resp, apiError(fmt.Errorf("apply %s %q: %w", o.kind, o.name, err))
		}
		c.s.log.Info("object applied", "kind", o.kind, "name", o.name, "action", action)
		resp.Results = append(resp.Results, &v1alpha1.ApplyResult{Kind: o.kind, Name: o.name, Action: action})
	}
	return resp, nil
}

// applySpec creates the object or replaces its spec, retrying on
// concurrent changes. It reports created, configured or unchanged.
func (s *Server) applySpec(ctx context.Context, kind, name string, spec []byte) (string, error) {
	for range casRetries {
		cur, err := s.store.Get(ctx, kind, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			_, err := s.store.Create(ctx, store.Record{Kind: kind, Name: name, Spec: spec})
			if errors.Is(err, store.ErrExists) {
				continue // created concurrently
			}
			return "created", err
		case err != nil:
			return "", err
		case bytes.Equal(cur.Spec, spec):
			return "unchanged", nil
		}
		_, err = s.store.Update(ctx, kind, name, spec, cur.Generation)
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			continue // changed or deleted concurrently: start over
		}
		return "configured", err
	}
	return "", status.Error(codes.Aborted, "too many concurrent changes, try again")
}

// routeOwners maps the host and path prefix of every stored Route to the
// Route's name.
func (s *Server) routeOwners(ctx context.Context) (map[string]string, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindRoute)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string, len(recs))
	for _, r := range recs {
		rt, err := objects.Route(r)
		if err != nil {
			return nil, err
		}
		owners[resource.RouteKey(rt.Spec)] = r.Name
	}
	return owners, nil
}

func (c *controlService) Get(ctx context.Context, req *v1alpha1.GetRequest) (*v1alpha1.Object, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := checkKind(req.GetKind()); err != nil {
		return nil, err
	}
	r, err := c.s.store.Get(ctx, req.GetKind(), req.GetName())
	if err != nil {
		return nil, apiError(err)
	}
	obj, err := c.toObject(r)
	return obj, apiError(err)
}

func (c *controlService) List(ctx context.Context, req *v1alpha1.ListRequest) (*v1alpha1.ListResponse, error) {
	if err := checkKind(req.GetKind()); err != nil {
		return nil, err
	}
	recs, err := c.s.store.List(ctx, req.GetKind())
	if err != nil {
		return nil, apiError(err)
	}
	resp := &v1alpha1.ListResponse{Items: make([]*v1alpha1.Object, 0, len(recs))}
	for _, r := range recs {
		obj, err := c.toObject(r)
		if err != nil {
			return nil, apiError(err)
		}
		resp.Items = append(resp.Items, obj)
	}
	return resp, nil
}

func (c *controlService) Delete(ctx context.Context, req *v1alpha1.DeleteRequest) (*v1alpha1.DeleteResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := checkKind(req.GetKind()); err != nil {
		return nil, err
	}
	if err := c.s.store.Delete(ctx, req.GetKind(), req.GetName(), 0); err != nil {
		return nil, apiError(err)
	}
	if req.GetKind() == v1alpha1.KindNode {
		// The node's certificate stays valid, but Sync and certificate
		// renewal refuse nodes without a Node object, so it must re-join.
		c.s.nodes.forget(req.GetName())
	}
	c.s.log.Info("object deleted", "kind", req.GetKind(), "name", req.GetName())
	return &v1alpha1.DeleteResponse{}, nil
}

func (c *controlService) CreateJoinToken(ctx context.Context, req *v1alpha1.CreateJoinTokenRequest) (*v1alpha1.CreateJoinTokenResponse, error) {
	ttl := req.GetTtl().AsDuration()
	if req.GetTtl() == nil || ttl == 0 {
		ttl = defaultTokenTTL
	}
	if ttl < time.Minute || ttl > maxTokenTTL {
		return nil, status.Errorf(codes.InvalidArgument, "ttl must be between 1m and %s", maxTokenTTL)
	}
	if req.GetMaxUses() < 0 {
		return nil, status.Error(codes.InvalidArgument, "maxUses must not be negative")
	}
	expires := c.s.now().Add(ttl)
	for range casRetries {
		id, secret, err := security.NewToken()
		if err != nil {
			return nil, apiError(err)
		}
		spec, err := encode(&v1alpha1.JoinTokenSpec{
			SecretHash:  security.HashSecret(secret),
			ExpiresAt:   ts(expires),
			MaxUses:     req.GetMaxUses(),
			Description: req.GetDescription(),
		})
		if err != nil {
			return nil, apiError(err)
		}
		_, err = c.s.store.Create(ctx, store.Record{Kind: v1alpha1.KindJoinToken, Name: id, Spec: spec})
		if errors.Is(err, store.ErrExists) {
			continue // id collision: pick another
		}
		if err != nil {
			return nil, apiError(err)
		}
		c.s.log.Info("join token created", "token", id, "expires", expires, "maxUses", req.GetMaxUses())
		return &v1alpha1.CreateJoinTokenResponse{
			Token:     id + "." + secret,
			CaHash:    security.CAHash(c.s.ca.Cert),
			ExpiresAt: ts(expires),
		}, nil
	}
	return nil, status.Error(codes.Internal, "could not allocate a token id")
}

func (c *controlService) toObject(r store.Record) (*v1alpha1.Object, error) {
	switch r.Kind {
	case v1alpha1.KindWorkload:
		w, err := objects.Workload(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Workload{Workload: w}}, nil
	case v1alpha1.KindNode:
		n, err := objects.Node(r)
		if err != nil {
			return nil, err
		}
		c.s.nodes.overlay(n)
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Node{Node: n}}, nil
	case v1alpha1.KindReplica:
		rp, err := objects.Replica(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Replica{Replica: rp}}, nil
	case v1alpha1.KindSecret:
		sec, err := objects.Secret(r)
		if err != nil {
			return nil, err
		}
		// Values never leave the server through the API: only the keys.
		for k := range sec.Spec.Data {
			sec.Spec.Data[k] = ""
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: sec}}, nil
	case v1alpha1.KindConfig:
		cfg, err := objects.Config(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Config{Config: cfg}}, nil
	case v1alpha1.KindService:
		svc, err := objects.Service(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Service{Service: svc}}, nil
	case v1alpha1.KindRoute:
		rt, err := objects.Route(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Route{Route: rt}}, nil
	}
	return nil, fmt.Errorf("unexpected kind %q", r.Kind)
}

// checkKind allows only user-visible kinds (never JoinToken). Deleting a
// Replica is allowed: the controller replaces it.
func checkKind(kind string) error {
	switch kind {
	case v1alpha1.KindWorkload, v1alpha1.KindNode, v1alpha1.KindReplica,
		v1alpha1.KindSecret, v1alpha1.KindConfig, v1alpha1.KindService, v1alpha1.KindRoute:
		return nil
	}
	return status.Errorf(codes.InvalidArgument, "unknown kind %q (want Workload, Replica, Node, Service, Route, Secret or Config)", kind)
}

// RestartWorkload sets spec.restartedAt, which changes the template and so
// replaces the replicas with a rolling update.
func (c *controlService) RestartWorkload(ctx context.Context, req *v1alpha1.RestartWorkloadRequest) (*v1alpha1.ApplyResult, error) {
	err := c.s.updateWorkloadSpec(ctx, req.GetName(), func(s *v1alpha1.WorkloadSpec) error {
		s.RestartedAt = ts(c.s.now())
		return nil
	})
	if err != nil {
		return nil, apiError(err)
	}
	c.s.log.Info("workload restart requested", "workload", req.GetName())
	return &v1alpha1.ApplyResult{Kind: v1alpha1.KindWorkload, Name: req.GetName(), Action: "restarted"}, nil
}
