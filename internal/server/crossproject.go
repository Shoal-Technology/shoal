package server

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// crossProjectErrors refuses each reference from an object to an object of the
// same kind that is not in the object's project but is in another project (ADR
// 0023, pass B1). References resolve only within a project. A reference to a
// name that no project holds is left to the checks that already exist (a
// missing Volume, for example), and a name that the object's own project holds
// is fine whatever other projects hold. Stored objects and objects of the same
// batch are both checked.
func (s *Server) crossProjectErrors(ctx context.Context, objs []prepared) ([]error, error) {
	stored := map[string][]store.Record{}
	listed := func(kind string) ([]store.Record, error) {
		if recs, ok := stored[kind]; ok {
			return recs, nil
		}
		recs, err := s.store.List(ctx, kind)
		if err != nil {
			return nil, err
		}
		stored[kind] = recs
		return recs, nil
	}
	var errs []error
	for _, o := range objs {
		if !tenancy.ProjectScoped(o.kind) {
			continue
		}
		for _, ref := range resource.References(o.kind, o.msg) {
			recs, err := listed(ref.Kind)
			if err != nil {
				return nil, err
			}
			projects := map[string]bool{}
			for _, r := range recs {
				p := tenancy.ProjectOf(r.Labels)
				if tenancy.Bare(r.Name, p) == ref.Name {
					projects[p] = true
				}
			}
			for _, b := range objs {
				if b.kind == ref.Kind && b.name == ref.Name {
					projects[b.project] = true
				}
			}
			if len(projects) == 0 || projects[o.project] {
				continue
			}
			target := slices.Sorted(maps.Keys(projects))[0]
			errs = append(errs, fmt.Errorf("%s %q in project %q references %s %q, which is in project %q: references resolve only within a project (ADR 0023): move the %s into project %q or change the reference",
				o.kind, o.name, o.project, ref.Kind, ref.Name, target, strings.ToLower(ref.Kind), o.project))
		}
	}
	return errs, nil
}
