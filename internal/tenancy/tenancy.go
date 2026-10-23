// Package tenancy holds the Project rules that the store, the API and the
// authorization code share (ADR 0023). It has no dependencies, so every
// package can use it.
package tenancy

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// DefaultProject holds every object that has no project label. It always
// exists and cannot be deleted.
const DefaultProject = "default"

// LabelProject is the reserved label that records the project of an object
// outside the default project. The server sets it. Users may not set it in
// metadata.labels; they set metadata.project instead.
const LabelProject = "shoal.dev/project"

// scoped are the kinds that belong to a project. Other kinds (Node, Project
// and the internal kinds) are cluster-wide. Pipeline, PipelineRun and Job
// are scoped from ADR 0023 pass B1 on.
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
	"Pipeline":      true,
	"PipelineRun":   true,
	"Job":           true,
	"Environment":   true,
}

// keySep joins an object name and its project in a store key. Object names
// and project names are DNS labels, which have no dots, so a key of the form
// name.project cannot be confused with a default key (a bare name).
const keySep = "."

// Key returns the store key of the object called name in project (ADR 0023,
// pass B1). The default project keeps the bare name, so records written
// before projects existed keep their keys. Any other project appends
// ".<project>". The key is also a valid DNS name, container ID and file name.
func Key(project, name string) string {
	if project == "" || project == DefaultProject {
		return name
	}
	return name + keySep + project
}

// Bare returns the object name of store key in project: the inverse of Key.
// A key that does not end in the project's suffix is returned unchanged.
func Bare(key, project string) string {
	if project == "" || project == DefaultProject {
		return key
	}
	return strings.TrimSuffix(key, keySep+project)
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

// KeyName returns the object name in a store key of a project-scoped kind.
// Object names and project names are DNS labels, so a key has at most one
// dot, and the name is the part before it.
func KeyName(key string) string {
	name, _, _ := strings.Cut(key, keySep)
	return name
}

// KeyProject returns the project in a store key of a project-scoped kind:
// the part after the dot, or the default project for a bare key.
func KeyProject(key string) string {
	if _, project, ok := strings.Cut(key, keySep); ok {
		return project
	}
	return DefaultProject
}
