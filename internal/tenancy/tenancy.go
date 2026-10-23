// Package tenancy holds the Project rules that the store, the API and the
// authorization code share (ADR 0023). It has no dependencies, so every
// package can use it.
package tenancy

import (
	"fmt"
	"maps"
	"regexp"
)

// DefaultProject holds every object that has no project label. It always
// exists and cannot be deleted.
const DefaultProject = "default"

// LabelProject is the reserved label that records the project of an object
// outside the default project. The server sets it. Users may not set it in
// metadata.labels; they set metadata.project instead.
const LabelProject = "shoal.dev/project"

// scoped are the kinds that belong to a project. Other kinds (Node, Project
// and the internal kinds) are cluster-wide.
var scoped = map[string]bool{
	"Workload":      true,
	"Service":       true,
	"Route":         true,
	"Volume":        true,
	"Database":      true,
	"BuildConfig":   true,
	"Build":         true,
	"Secret":        true,
	"Config":        true,
	"ResourceQuota": true,
}

// ProjectScoped reports whether objects of kind belong to a project.
func ProjectScoped(kind string) bool {
	return scoped[kind]
}

// ScopedKinds returns the project-scoped kinds, in no particular order.
func ScopedKinds() []string {
	out := make([]string, 0, len(scoped))
	for k := range scoped {
		out = append(out, k)
	}
	return out
}

// ProjectOf returns the project of a stored record from its labels. A record
// without the project label is in the default project, so records written
// before projects existed need no migration.
func ProjectOf(labels map[string]string) string {
	if p := labels[LabelProject]; p != "" {
		return p
	}
	return DefaultProject
}

// WithProject returns a copy of labels that carries project. The default
// project has no label, so a default object's labels are unchanged. The
// reserved label in labels is replaced, never copied.
func WithProject(labels map[string]string, project string) map[string]string {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	delete(out, LabelProject)
	if project != "" && project != DefaultProject {
		out[LabelProject] = project
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WithoutProject returns a copy of labels with the reserved label removed.
// It is used for the labels that the API returns, so that a manifest read
// back from the API can be applied again.
func WithoutProject(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	delete(out, LabelProject)
	return out
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name is a valid project name: a lowercase DNS
// label of at most 63 characters.
func ValidName(name string) bool {
	return len(name) <= 63 && nameRE.MatchString(name)
}

// CheckName returns an error naming the rule that name breaks.
func CheckName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("project name %q: must be a lowercase DNS label of at most 63 characters", name)
	}
	return nil
}
