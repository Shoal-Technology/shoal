package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/authz"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// prepared is one object of an Apply, after validation.
type prepared struct {
	kind, name string
	spec       []byte
	msg        proto.Message
	labels     map[string]string
	// requested is metadata.project as the client sent it. Empty keeps the
	// object's current project.
	requested string
	// project and exists are set by resolveProject.
	project string
	exists  bool
}

// resolveProject sets the project an object ends in, and whether the object
// is stored already. A user error (a bad project, or a move between
// projects) is returned as the first error. The second error is a store
// failure. Cluster-wide kinds have no project.
func (s *Server) resolveProject(ctx context.Context, o *prepared) (userErr, err error) {
	if !tenancy.ProjectScoped(o.kind) {
		if o.requested != "" {
			return fmt.Errorf("%s %q: metadata.project applies only to project-scoped kinds", o.kind, o.name), nil
		}
		return nil, nil
	}
	cur, err := s.store.Get(ctx, o.kind, o.name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		o.project = o.requested
		if o.project == "" {
			o.project = tenancy.DefaultProject
		}
	case err != nil:
		return nil, err
	default:
		o.exists = true
		o.project = tenancy.ProjectOf(cur.Labels)
		if o.requested != "" && o.requested != o.project {
			return fmt.Errorf("%s %q is in project %q: move it by deleting it and applying it again, not by setting metadata.project to %q",
				o.kind, o.name, o.project, o.requested), nil
		}
	}
	ok, err := s.projectExists(ctx, o.project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return fmt.Errorf("%s %q: project %q not found (apply the Project first)", o.kind, o.name, o.project), nil
	}
	o.labels = tenancy.WithProject(o.labels, o.project)
	return nil, nil
}

// projectExists reports whether a project exists. The default project always
// does, whether or not it has been applied.
func (s *Server) projectExists(ctx context.Context, name string) (bool, error) {
	if name == tenancy.DefaultProject {
		return true, nil
	}
	_, err := s.store.Get(ctx, v1alpha1.KindProject, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// recordProject returns the project of a stored project-scoped object. found
// is false for a cluster-wide kind and for an object that is not stored.
func (s *Server) recordProject(ctx context.Context, kind, name string) (project string, found bool, err error) {
	if !tenancy.ProjectScoped(kind) {
		return "", false, nil
	}
	r, err := s.store.Get(ctx, kind, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return tenancy.ProjectOf(r.Labels), true, nil
}

// projectObjectCount counts the project-scoped objects in project, quotas
// included.
func (s *Server) projectObjectCount(ctx context.Context, project string) (int, error) {
	n := 0
	for _, kind := range tenancy.ScopedKinds() {
		recs, err := s.store.List(ctx, kind)
		if err != nil {
			return 0, err
		}
		for _, r := range recs {
			if tenancy.ProjectOf(r.Labels) == project {
				n++
			}
		}
	}
	return n, nil
}

// deleteProject removes a project that holds no objects. The default project
// cannot be deleted.
func (s *Server) deleteProject(ctx context.Context, name string) error {
	if name == tenancy.DefaultProject {
		return status.Error(codes.FailedPrecondition, "the default project cannot be deleted")
	}
	n, err := s.projectObjectCount(ctx, name)
	if err != nil {
		return apiError(err)
	}
	if n > 0 {
		return status.Errorf(codes.FailedPrecondition, "project %q still has %d objects: delete them first", name, n)
	}
	return apiError(s.store.Delete(ctx, v1alpha1.KindProject, name, 0))
}

// defaultProjectObject is the default project as the API shows it when it is
// not stored.
func defaultProjectObject() *v1alpha1.Object {
	return &v1alpha1.Object{Kind: &v1alpha1.Object_Project{Project: &v1alpha1.Project{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindProject,
		Metadata:   &v1alpha1.ObjectMeta{Name: tenancy.DefaultProject},
		Spec: &v1alpha1.ProjectSpec{
			Description: "Objects created before projects existed, and objects applied without a project.",
		},
	}}}
}

// quotaLimit is one stored ResourceQuota and its parsed limits.
type quotaLimit struct {
	name   string
	limits resource.Limits
}

// quotasByProject groups the stored quotas by project.
func (s *Server) quotasByProject(ctx context.Context) (map[string][]quotaLimit, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindResourceQuota)
	if err != nil {
		return nil, err
	}
	out := map[string][]quotaLimit{}
	for _, r := range recs {
		q, err := objects.ResourceQuota(r)
		if err != nil {
			return nil, err
		}
		lim, err := resource.ParseLimits(q.GetSpec())
		if err != nil {
			return nil, fmt.Errorf("quota %q: %w", r.Name, err)
		}
		p := tenancy.ProjectOf(r.Labels)
		out[p] = append(out[p], quotaLimit{name: r.Name, limits: lim})
	}
	return out, nil
}

// checkQuotas refuses the batch when it takes a project past one of its
// quotas (ADR 0023, section 7). It runs before anything is written. Only the
// projects that the batch adds Workloads or Volumes to are checked.
func (s *Server) checkQuotas(ctx context.Context, objs []prepared) error {
	quotas, err := s.quotasByProject(ctx)
	if err != nil {
		return apiError(err)
	}
	if len(quotas) == 0 {
		return nil
	}
	touched := map[string]bool{}
	for _, o := range objs {
		if quotaKind(o.kind) && quotas[o.project] != nil {
			touched[o.project] = true
		}
	}
	var errs []error
	for _, p := range slices.Sorted(maps.Keys(touched)) {
		before, after, err := s.projectUsage(ctx, p, objs)
		if err != nil {
			return apiError(err)
		}
		for _, q := range quotas[p] {
			if err := resource.CheckQuota(p, q.name, q.limits, before, after); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return nil
}

// quotaKind reports whether objects of kind use quota.
func quotaKind(kind string) bool {
	return kind == v1alpha1.KindWorkload || kind == v1alpha1.KindVolume
}

// projectUsage returns the usage of project now, and the usage once the batch
// is applied. A batch object replaces the stored object of the same kind and
// name.
func (s *Server) projectUsage(ctx context.Context, project string, objs []prepared) (before, after resource.Usage, err error) {
	stored := map[string]resource.Usage{}
	for _, kind := range []string{v1alpha1.KindWorkload, v1alpha1.KindVolume} {
		recs, err := s.store.List(ctx, kind)
		if err != nil {
			return before, after, err
		}
		for _, r := range recs {
			if tenancy.ProjectOf(r.Labels) != project {
				continue
			}
			spec, err := storedSpec(r)
			if err != nil {
				return before, after, err
			}
			u, err := resource.UsageOf(spec)
			if err != nil {
				return before, after, fmt.Errorf("%s %q: %w", kind, r.Name, err)
			}
			stored[kind+"/"+r.Name] = u
			before = before.Add(u)
		}
	}
	after = before
	for _, o := range objs {
		if o.project != project || !quotaKind(o.kind) {
			continue
		}
		u, err := resource.UsageOf(o.msg)
		if err != nil {
			return before, after, fmt.Errorf("%s %q: %w", o.kind, o.name, err)
		}
		key := o.kind + "/" + o.name
		after = after.Sub(stored[key]).Add(u)
		stored[key] = u
	}
	return before, after, nil
}

// storedSpec decodes the spec of a stored Workload or Volume.
func storedSpec(r store.Record) (proto.Message, error) {
	switch r.Kind {
	case v1alpha1.KindWorkload:
		w, err := objects.Workload(r)
		if err != nil {
			return nil, err
		}
		return w.GetSpec(), nil
	case v1alpha1.KindVolume:
		v, err := objects.Volume(r)
		if err != nil {
			return nil, err
		}
		return v.GetSpec(), nil
	}
	return nil, fmt.Errorf("no spec for kind %q", r.Kind)
}

// controlAuthz maps each Control method to the verb and kind it needs, for
// the calls that do not name objects. An empty verb means the handler checks
// its objects itself (Apply, Get, List and Delete; ADR 0023, section 6). A
// method not in the table needs cluster-admin.
var controlAuthz = map[string]struct{ verb, kind string }{
	"Apply":           {},
	"Get":             {},
	"List":            {},
	"Delete":          {},
	"SetNodeLabels":   {authz.VerbUpdate, v1alpha1.KindNode},
	"CreateJoinToken": {authz.VerbAdmin, ""},
	"Logs":            {authz.VerbGet, v1alpha1.KindWorkload},
	"RestartWorkload": {authz.VerbUpdate, v1alpha1.KindWorkload},
	"ResetUIPassword": {authz.VerbAdmin, ""},
	"JoinCluster":     {authz.VerbAdmin, ""},
	"ClusterStatus":   {authz.VerbGet, ""},
	"RemoveServer":    {authz.VerbAdmin, ""},
	"JoinBundle":      {authz.VerbAdmin, ""},
	"Backup":          {authz.VerbAdmin, ""},
	"DatabaseBackups": {authz.VerbGet, v1alpha1.KindDatabase},
	"BackupDatabase":  {authz.VerbUpdate, v1alpha1.KindDatabase},
	"StartBuild":      {authz.VerbCreate, v1alpha1.KindBuild},
	"CancelBuild":     {authz.VerbUpdate, v1alpha1.KindBuild},
	"BuildLogs":       {authz.VerbGet, v1alpha1.KindBuild},
}

const controlPrefix = "/shoal.v1alpha1.Control/"

// authzRPC checks a Control call that has no object checks of its own. It
// runs after the certificate role check (auth.go).
func (s *Server) authzRPC(ctx context.Context, fullMethod string) error {
	if !strings.HasPrefix(fullMethod, controlPrefix) {
		return nil
	}
	method := fullMethod[strings.LastIndex(fullMethod, "/")+1:]
	entry, ok := controlAuthz[method]
	if ok && entry.verb == "" {
		return nil
	}
	if !ok {
		entry.verb, entry.kind = authz.VerbAdmin, ""
	}
	return s.authzCheck(ctx, authzCall{Method: method, Verb: entry.verb, Kind: entry.kind})
}

// authzCall describes one request to decide.
type authzCall struct {
	Method, Verb, Kind, Project, Name string
}

// authzCheck decides a call and records the decision. In audit mode a denial
// is logged and the call goes on. In enforce mode it is refused. Mutating
// calls are logged whether they are allowed or not.
func (s *Server) authzCheck(ctx context.Context, c authzCall) error {
	subject := authz.Subject{}
	actor := "unauthenticated"
	if cl, ok := callerFrom(ctx); ok {
		subject = authz.Subject{User: cl.id.Name, CertRole: cl.id.Role}
		actor = cl.id.Name + " (" + cl.id.Role + ")"
	}
	d := authz.Decide(authz.Request{Subject: subject, Verb: c.Verb, Kind: c.Kind, Project: c.Project}, s.authzBindings())
	attrs := []any{
		"method", c.Method, "verb", c.Verb, "kind", c.Kind, "project", c.Project, "name", c.Name,
		"actor", actor, "allowed", d.Allowed, "reason", d.Reason, "mode", string(s.authzMode),
	}
	switch {
	case s.authzMode.Blocks(d):
		s.log.Warn("authz denied", attrs...)
		return status.Errorf(codes.PermissionDenied, "%s: %s", actor, d.Reason)
	case !d.Allowed:
		s.log.Warn("authz would deny (audit mode)", attrs...)
	case authz.IsMutating(c.Verb):
		s.log.Info("audit", attrs...)
	}
	return nil
}

// authzBindings returns the stored role bindings. Pass A stores none (ADR
// 0023, section 5), so only the admin certificate is allowed.
func (s *Server) authzBindings() []authz.Binding {
	return nil
}
