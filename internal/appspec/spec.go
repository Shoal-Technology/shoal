// Package appspec parses shoal.yaml, the app spec of ADR 0017 (section 2),
// and expands it into the objects that Shoal stores: Workload, Service,
// Route and Volume. Expansion is a pure function. Nothing here talks to a
// server, so the CLI can show the plan offline.
package appspec

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v2"

	"github.com/Shoal-Technology/shoal/internal/resource"
)

// MaxAppNameLen bounds App.Name. Expansion adds suffixes such as
// "-db-credentials" and "-<volume>", and every result must fit in a
// 63-character DNS label.
const MaxAppNameLen = 40

// DatabaseEngine is the only engine accepted in v1.
const DatabaseEngine = "postgres"

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// versionRun is a major or major.minor version, such as "16" or "16.15".
	versionRun = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// App is a parsed shoal.yaml.
type App struct {
	Name   string `yaml:"name"`
	Image  string `yaml:"image"`
	Port   int    `yaml:"port"`
	Domain string `yaml:"domain"`
	// Replicas is nil when the key is absent; the default is 1.
	Replicas *int              `yaml:"replicas"`
	Env      map[string]string `yaml:"env"`
	// Secrets lists keys of the Secret <name>-secrets. Each one becomes an
	// environment variable with the same name.
	Secrets   []string  `yaml:"secrets"`
	Database  Database  `yaml:"database"`
	Volumes   []Volume  `yaml:"volumes"`
	Health    *Health   `yaml:"health"`
	Resources Resources `yaml:"resources"`
	// Build is reserved. Any value is rejected: Shoal does not build images
	// in v1 (ADR 0017, open question 2).
	Build any `yaml:"build"`
}

// Database is either the engine name alone (`database: postgres`) or a
// mapping with engine, version and size.
type Database struct {
	Engine  string `yaml:"engine"`
	Version string `yaml:"version"`
	Size    string `yaml:"size"`
}

// UnmarshalYAML accepts the short string form as well as the mapping.
func (d *Database) UnmarshalYAML(unmarshal func(any) error) error {
	var engine string
	if err := unmarshal(&engine); err == nil {
		*d = Database{Engine: engine}
		return nil
	}
	type plain Database
	var p plain
	if err := unmarshal(&p); err != nil {
		return err
	}
	*d = Database(p)
	return nil
}

// Volume is node-local storage mounted at MountPath.
type Volume struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	Size      string `yaml:"size"`
}

// Health is an HTTP readiness check on the container port.
type Health struct {
	Path string `yaml:"path"`
}

// Resources are the CPU and memory limits, in the forms "200m" and "256Mi".
type Resources struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// Parse decodes a shoal.yaml document strictly and validates it. An unknown
// key is an error, and the error names its line.
func Parse(data []byte) (*App, error) {
	var app App
	if err := yaml.UnmarshalStrict(data, &app); err != nil {
		return nil, fmt.Errorf("shoal.yaml: %w", err)
	}
	if err := Validate(&app); err != nil {
		return nil, err
	}
	return &app, nil
}

// replicaCount returns the effective replica count.
func (a *App) replicaCount() int {
	if a.Replicas == nil {
		return 1
	}
	return *a.Replicas
}

// checkFields reports every problem in the app spec that has a clear field
// name. Limits enforced by the resource package (image reference, host,
// resource quantities, volume size and mount paths) are reported by Expand,
// through the object validation the server also runs.
func checkFields(app *App) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	switch {
	case app.Name == "":
		bad("name", "required")
	case !resource.ValidName(app.Name, MaxAppNameLen):
		bad("name", "must be a lowercase DNS label of at most %d characters", MaxAppNameLen)
	}
	if app.Build != nil {
		bad("build", "build is not supported; push an image from CI and set image (see docs)")
	}
	if app.Image == "" {
		bad("image", "required")
	}
	if app.Port < 0 || app.Port > 65535 {
		bad("port", "must be between 1 and 65535")
	}
	if app.Domain != "" && app.Port == 0 {
		bad("domain", "needs port: the container port that the domain routes to")
	}
	if h := app.Health; h != nil {
		if app.Port == 0 {
			bad("health", "needs port: the probe runs on the container port")
		}
		if !strings.HasPrefix(h.Path, "/") {
			bad("health.path", "must start with /")
		}
	}
	if app.Replicas != nil && (*app.Replicas < 0 || *app.Replicas > resource.MaxReplicas) {
		bad("replicas", "must be between 0 and %d", resource.MaxReplicas)
	}
	if len(app.Volumes) > 0 && app.replicaCount() > 1 {
		bad("replicas", "must be 1 when volumes are set (a volume is read-write), not %d", app.replicaCount())
	}

	hasDB := app.Database != (Database{})
	if hasDB {
		if app.Database.Engine == "" {
			bad("database.engine", "required when database is set")
		} else if app.Database.Engine != DatabaseEngine {
			bad("database.engine", "must be %s, the only engine in v1", DatabaseEngine)
		}
		if v := app.Database.Version; v != "" && !versionRun.MatchString(v) {
			bad("database.version", "must be a major or major.minor version such as \"16\"")
		}
		if s := app.Database.Size; s != "" {
			if _, err := resource.ParseMemory(s); err != nil {
				bad("database.size", "%v", err)
			}
		}
	}

	envKeys := make([]string, 0, len(app.Env))
	for k := range app.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		f := "env." + k
		switch {
		case !envName.MatchString(k):
			bad(f, "invalid environment variable name")
		case hasDB && k == DatabaseURLEnv:
			bad(f, "%s is set by database: remove it from env", DatabaseURLEnv)
		}
	}

	seenSecret := map[string]bool{}
	for i, s := range app.Secrets {
		f := fmt.Sprintf("secrets[%d]", i)
		switch {
		case !envName.MatchString(s):
			bad(f, "invalid key %q: it becomes an environment variable name", s)
		case seenSecret[s]:
			bad(f, "duplicate %q", s)
		case hasKey(app.Env, s):
			bad(f, "%q is also set in env", s)
		case hasDB && s == DatabaseURLEnv:
			bad(f, "%s is set by database: remove it from secrets", DatabaseURLEnv)
		}
		seenSecret[s] = true
	}

	seenVolume := map[string]bool{}
	for i, v := range app.Volumes {
		f := fmt.Sprintf("volumes[%d]", i)
		switch {
		case v.Name == "":
			bad(f+".name", "required")
		case !resource.ValidName(v.Name, 63-len(app.Name)-1):
			bad(f+".name", "must be a lowercase DNS label; with the app name it must fit in 63 characters")
		case seenVolume[v.Name]:
			bad(f+".name", "duplicate %q", v.Name)
		}
		seenVolume[v.Name] = true
		if v.MountPath == "" {
			bad(f+".mountPath", "required")
		}
		if v.Size == "" {
			bad(f+".size", "required, for example 5Gi")
		}
	}

	return errors.Join(errs...)
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
