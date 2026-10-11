package server

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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
	// stored or earlier in this batch. This read-only check gives the clear
	// message and covers Routes stored before claims existed. It is not
	// atomic, so the claim reservation below is the real check (ADR 0015,
	// pass E).
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

	// Volume claims taken by this apply, per workload, until it is applied.
	claimed := map[string][]string{}
	releaseVolClaims := func() {
		for w, vols := range claimed {
			if err := c.s.releaseVolumes(ctx, w, vols); err != nil {
				c.s.log.Warn("could not release volume claims", "workload", w, "err", err)
			}
		}
	}

	// Reserve each Route's host and path prefix. Create is atomic, so when
	// two applies race for one key exactly one reservation succeeds; the other
	// gets a conflict and nothing is written for it. claims maps each key this
	// call created to its Route, so the reservations can be undone on failure.
	claims := map[string]string{}
	for _, o := range objs {
		if o.kind != v1alpha1.KindRoute {
			continue
		}
		key := resource.RouteKey(o.msg.(*v1alpha1.RouteSpec))
		created, err := c.s.reserveRoute(ctx, o.name, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if created {
			claims[key] = o.name
		}
	}
	if err := errors.Join(errs...); err != nil {
		c.s.releaseClaims(ctx, claims)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Any failure before the writes are done releases what this call
	// reserved: Route keys and Volume claims. Releasing is idempotent (each
	// release checks the owner), so the write-phase releases may run again.
	ok := false
	defer func() {
		if !ok {
			c.s.releaseClaims(ctx, claims)
			releaseVolClaims()
		}
	}()

	// Volumes (ADR 0016, section 1.1). Every Volume a Workload names must
	// exist, stored or in this batch, and a bound Volume cannot be resized.
	// Then each Workload takes its claims before anything is written. A claim
	// that fails releases the claims this apply already took.
	batchVolumes := map[string]bool{}
	for _, o := range objs {
		if o.kind == v1alpha1.KindVolume {
			batchVolumes[o.name] = true
		}
	}
	var volErrs []error
	prevVolumes := map[string][]string{} // workload -> volumes in its stored spec
	for _, o := range objs {
		switch o.kind {
		case v1alpha1.KindWorkload:
			names := volumeNames(o.msg.(*v1alpha1.WorkloadSpec))
			missing, err := c.s.missingVolumes(ctx, names, batchVolumes)
			if err != nil {
				return nil, apiError(err)
			}
			if len(missing) > 0 {
				volErrs = append(volErrs, fmt.Errorf("workload %q: volumes %v not found (apply the Volume first)", o.name, missing))
			}
			if prevVolumes[o.name], err = c.s.storedVolumeNames(ctx, o.name); err != nil {
				return nil, apiError(err)
			}
		case v1alpha1.KindVolume:
			rec, err := c.s.store.Get(ctx, v1alpha1.KindVolume, o.name)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return nil, apiError(err)
			default:
				cur, err := objects.Volume(rec)
				if err != nil {
					return nil, apiError(err)
				}
				if err := checkVolumeResize(cur, o.msg.(*v1alpha1.VolumeSpec)); err != nil {
					volErrs = append(volErrs, err)
				}
			}
		}
	}
	if err := errors.Join(volErrs...); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	for _, o := range objs {
		if o.kind != v1alpha1.KindWorkload {
			continue
		}
		vols, err := c.s.claimVolumes(ctx, o.name, volumeNames(o.msg.(*v1alpha1.WorkloadSpec)))
		claimed[o.name] = vols
		if err != nil {
			return nil, apiError(err)
		}
	}

	resp := &v1alpha1.ApplyResponse{}
	for _, o := range objs {
		var oldKey, newKey string
		if o.kind == v1alpha1.KindRoute {
			newKey = resource.RouteKey(o.msg.(*v1alpha1.RouteSpec))
			if oldKey, err = c.s.storedRouteKey(ctx, o.name); err != nil {
				c.s.releaseClaims(ctx, claims)
				return resp, apiError(err)
			}
		}
		action, err := c.s.applySpec(ctx, o.kind, o.name, o.spec)
		if err != nil {
			c.s.releaseClaims(ctx, claims)
			releaseVolClaims()
			return resp, apiError(fmt.Errorf("apply %s %q: %w", o.kind, o.name, err))
		}
		if o.kind == v1alpha1.KindRoute {
			delete(claims, newKey)
			// The Route moved to another host or prefix: free the old key.
			if oldKey != "" && oldKey != newKey {
				c.s.releaseRoute(ctx, oldKey, o.name)
			}
		}
		if o.kind == v1alpha1.KindWorkload {
			delete(claimed, o.name)
			// Volumes the workload no longer names are released.
			newNames := volumeNames(o.msg.(*v1alpha1.WorkloadSpec))
			var dropped []string
			for _, v := range prevVolumes[o.name] {
				if !slices.Contains(newNames, v) {
					dropped = append(dropped, v)
				}
			}
			if err := c.s.releaseVolumes(ctx, o.name, dropped); err != nil {
				c.s.log.Warn("could not release dropped volume claims", "workload", o.name, "err", err)
			}
		}
		c.s.log.Info("object applied", "kind", o.kind, "name", o.name, "action", action)
		resp.Results = append(resp.Results, &v1alpha1.ApplyResult{Kind: o.kind, Name: o.name, Action: action})
	}
	ok = true
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

// routeOrder orders Route records oldest first: createdAt, then revision (the
// store's write order), then name. The backfill and the edge snapshot both use
// it, so they agree on which of two legacy Routes on one key is the older.
func routeOrder(a, b store.Record) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Revision, b.Revision); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}

// backfillRouteClaims gives every stored Route its claim (ADR 0015, pass E).
// Routes stored before claims existed have none, so reserveRoute cannot see
// them. Routes are visited oldest first (createdAt, then revision, then name),
// so the oldest Route on a key keeps the claim. A newer Route whose key is held
// by another Route gets no claim; the controller reports it as Conflict from
// the claim records. Running it again changes nothing.
func (s *Server) backfillRouteClaims(ctx context.Context) (created, conflicts int, err error) {
	recs, err := s.store.List(ctx, v1alpha1.KindRoute)
	if err != nil {
		return 0, 0, err
	}
	slices.SortFunc(recs, routeOrder)
	for _, r := range recs {
		rt, err := objects.Route(r)
		if err != nil {
			return created, conflicts, err
		}
		key := resource.RouteKey(rt.Spec)
		cur, err := s.store.Get(ctx, v1alpha1.KindRouteClaim, key)
		switch {
		case err == nil && string(cur.Spec) == r.Name:
			continue // already claimed by this Route
		case err == nil:
			conflicts++
			s.log.Warn("route not claimed: host and pathPrefix held by an older route", "route", r.Name, "key", key, "holder", string(cur.Spec))
			continue
		case !errors.Is(err, store.ErrNotFound):
			return created, conflicts, err
		}
		ok, err := s.reserveRoute(ctx, r.Name, key)
		if err != nil {
			// Lost a race with an apply that claimed the key meanwhile.
			conflicts++
			s.log.Warn("route not claimed", "route", r.Name, "key", key, "err", err)
			continue
		}
		if ok {
			created++
		}
	}
	return created, conflicts, nil
}

// reserveRoute claims key for the named Route (ADR 0015, pass E). It reports
// whether this call created the claim. A claim the Route already holds is
// kept, and a claim held by another Route is a conflict. Create is atomic,
// so of two racing reservations for one key exactly one succeeds.
func (s *Server) reserveRoute(ctx context.Context, name, key string) (bool, error) {
	for range casRetries {
		_, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindRouteClaim, Name: key, Spec: []byte(name)})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrExists) {
			return false, err
		}
		cur, err := s.store.Get(ctx, v1alpha1.KindRouteClaim, key)
		if errors.Is(err, store.ErrNotFound) {
			continue // released meanwhile: try again
		}
		if err != nil {
			return false, err
		}
		if string(cur.Spec) == name {
			return false, nil
		}
		return false, fmt.Errorf("route %q: host and pathPrefix are already used by route %q", name, string(cur.Spec))
	}
	return false, status.Error(codes.Aborted, "too many concurrent changes, try again")
}

// releaseRoute frees key if the named Route holds it. The delete is
// conditional on the claim's generation, so it never frees another Route's claim.
func (s *Server) releaseRoute(ctx context.Context, key, name string) {
	cur, err := s.store.Get(ctx, v1alpha1.KindRouteClaim, key)
	if err != nil || string(cur.Spec) != name {
		return
	}
	if err := s.store.Delete(ctx, v1alpha1.KindRouteClaim, key, cur.Generation); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		s.log.Warn("release route claim", "route", name, "key", key, "err", err)
	}
}

// releaseClaims frees the reservations made by a failed apply (key to Route).
func (s *Server) releaseClaims(ctx context.Context, claims map[string]string) {
	for key, name := range claims {
		s.releaseRoute(ctx, key, name)
	}
}

// storedRouteKey returns the host and path prefix key of a stored Route, or
// "" when there is no such Route.
func (s *Server) storedRouteKey(ctx context.Context, name string) (string, error) {
	rec, err := s.store.Get(ctx, v1alpha1.KindRoute, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rt, err := objects.Route(rec)
	if err != nil {
		return "", err
	}
	return resource.RouteKey(rt.Spec), nil
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
	if req.GetKind() == v1alpha1.KindVolume {
		if err := c.deleteVolume(ctx, req.GetName()); err != nil {
			return nil, err
		}
		c.s.log.Info("object deleted", "kind", req.GetKind(), "name", req.GetName())
		return &v1alpha1.DeleteResponse{}, nil
	}
	var routeKey string
	if req.GetKind() == v1alpha1.KindRoute {
		key, err := c.s.storedRouteKey(ctx, req.GetName())
		if err != nil {
			return nil, apiError(err)
		}
		routeKey = key
	}
	var volumes []string
	if req.GetKind() == v1alpha1.KindWorkload {
		var err error
		if volumes, err = c.s.storedVolumeNames(ctx, req.GetName()); err != nil {
			return nil, apiError(err)
		}
	}
	if err := c.s.store.Delete(ctx, req.GetKind(), req.GetName(), 0); err != nil {
		return nil, apiError(err)
	}
	if routeKey != "" {
		c.s.releaseRoute(ctx, routeKey, req.GetName())
	}
	if len(volumes) > 0 {
		if err := c.s.releaseVolumes(ctx, req.GetName(), volumes); err != nil {
			c.s.log.Warn("could not release volume claims", "workload", req.GetName(), "err", err)
		}
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
	case v1alpha1.KindVolume:
		v, err := objects.Volume(r)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.Object{Kind: &v1alpha1.Object_Volume{Volume: v}}, nil
	}
	return nil, fmt.Errorf("unexpected kind %q", r.Kind)
}

// checkKind allows only user-visible kinds (never JoinToken). Deleting a
// Replica is allowed: the controller replaces it.
func checkKind(kind string) error {
	switch kind {
	case v1alpha1.KindWorkload, v1alpha1.KindNode, v1alpha1.KindReplica,
		v1alpha1.KindSecret, v1alpha1.KindConfig, v1alpha1.KindService, v1alpha1.KindRoute, v1alpha1.KindVolume:
		return nil
	}
	return status.Errorf(codes.InvalidArgument, "unknown kind %q (want Workload, Replica, Node, Service, Route, Volume, Secret or Config)", kind)
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
