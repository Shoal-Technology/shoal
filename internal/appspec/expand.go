package appspec

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/resource"
)

// Object is one stored object, as the server and the CLI see it.
type Object = v1alpha1.Object

// LabelApp is the label that every expanded object carries (ADR 0017, 2.3).
const LabelApp = "app"

// DatabaseURLEnv is the environment variable that carries the database URL.
const DatabaseURLEnv = "DATABASE_URL"

// Names of the objects that an app expands to.
const (
	databaseCredentialsKey = "url"
	routeServicePort       = 80
	healthIntervalSeconds  = 10
	healthTimeoutSeconds   = 2
	healthFailureThreshold = 3
)

// Defaults for database: postgres (ADR 0017, 2.2). The memory default fits a
// 1 GiB host next to the server and the agent; ADR 0016 section 3.1 is
// updated to match in pass C.
const (
	DefaultDatabaseVersion = "16"
	DefaultDatabaseSize    = "5Gi"
	DefaultDatabaseCPU     = "500m"
	DefaultDatabaseMemory  = "384Mi"
)

// valueOr returns v, or def when v is empty.
func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// DatabaseName is the name of the Database object of app (ADR 0017, 2.3).
func DatabaseName(app string) string { return app + "-db" }

// DatabaseCredentialsName is the Secret that the Database controller creates
// with the connection URL under the key "url".
func DatabaseCredentialsName(app string) string { return DatabaseName(app) + "-credentials" }

// SecretsName is the Secret that holds the values of app.Secrets.
func SecretsName(app string) string { return app + "-secrets" }

// VolumeName is the name of the Volume object for a volume of app.
func VolumeName(app, volume string) string { return app + "-" + volume }

// Expand checks app and returns the objects it stands for, in this order:
// Workload; Service (when port is set); Route (when domain is set); Database
// (when database is set); then one Volume per entry of volumes. The same app
// always gives the same objects.
//
// The Secret <name>-secrets is not emitted: deploy creates it empty, and its
// values are set with `shoal app secret set`. The credential Secret
// <name>-db-credentials is created by the Database controller, so only the
// env reference to it is emitted here.
func Expand(app *App) ([]*Object, error) {
	if err := checkFields(app); err != nil {
		return nil, err
	}
	objs := build(app)
	for _, o := range objs {
		if o.GetDatabase() != nil {
			// resource.Prepare has no Database case yet. checkFields
			// validates the engine, version and size instead.
			continue
		}
		kind, name, _, err := resource.Prepare(o)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", kind, name, err)
		}
	}
	return objs, nil
}

// Validate reports whether app is valid, with the same errors as Expand.
func Validate(app *App) error {
	_, err := Expand(app)
	return err
}

// meta returns the metadata of an object of app, with the app label.
func meta(app, name string) *v1alpha1.ObjectMeta {
	return &v1alpha1.ObjectMeta{Name: name, Labels: map[string]string{LabelApp: app}}
}

// build constructs the objects. It assumes app has passed checkFields.
func build(app *App) []*Object {
	var out []*Object

	out = append(out, &Object{Kind: &v1alpha1.Object_Workload{Workload: &v1alpha1.Workload{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkload,
		Metadata:   meta(app.Name, app.Name),
		Spec:       workloadSpec(app),
	}}})

	if app.Port > 0 {
		out = append(out, &Object{Kind: &v1alpha1.Object_Service{Service: &v1alpha1.Service{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindService,
			Metadata:   meta(app.Name, app.Name),
			Spec: &v1alpha1.ServiceSpec{
				Selector: app.Name,
				Ports: []*v1alpha1.ServicePort{{
					Port:       routeServicePort,
					TargetPort: int32(app.Port),
					Protocol:   "tcp",
				}},
			},
		}}})
	}

	if app.Domain != "" {
		out = append(out, &Object{Kind: &v1alpha1.Object_Route{Route: &v1alpha1.Route{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindRoute,
			Metadata:   meta(app.Name, app.Name),
			Spec: &v1alpha1.RouteSpec{
				Host:       app.Domain,
				PathPrefix: "/",
				Service:    app.Name,
				Port:       routeServicePort,
				Tls:        "auto",
			},
		}}})
	}

	if app.Database != (Database{}) {
		out = append(out, &Object{Kind: &v1alpha1.Object_Database{Database: &v1alpha1.Database{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindDatabase,
			Metadata:   meta(app.Name, DatabaseName(app.Name)),
			Spec: &v1alpha1.DatabaseSpec{
				Engine:    DatabaseEngine,
				Version:   valueOr(app.Database.Version, DefaultDatabaseVersion),
				Size:      valueOr(app.Database.Size, DefaultDatabaseSize),
				Resources: &v1alpha1.Resources{Cpu: DefaultDatabaseCPU, Memory: DefaultDatabaseMemory},
			},
		}}})
	}

	for _, v := range app.Volumes {
		out = append(out, &Object{Kind: &v1alpha1.Object_Volume{Volume: &v1alpha1.Volume{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindVolume,
			Metadata:   meta(app.Name, VolumeName(app.Name, v.Name)),
			Spec: &v1alpha1.VolumeSpec{
				Size:       v.Size,
				AccessMode: resource.AccessRWO,
				Retention:  resource.RetentionRetain,
			},
		}}})
	}
	return out
}

// workloadSpec builds the Workload spec of app.
func workloadSpec(app *App) *v1alpha1.WorkloadSpec {
	s := &v1alpha1.WorkloadSpec{
		Image:    app.Image,
		Replicas: proto.Int32(int32(app.replicaCount())),
		Env:      envVars(app),
	}
	if app.Port > 0 {
		s.Ports = []*v1alpha1.Port{{ContainerPort: int32(app.Port)}}
	}
	if h := app.Health; h != nil {
		s.HealthCheck = &v1alpha1.HealthCheck{
			Http:             &v1alpha1.HTTPHealthCheck{Path: h.Path, Port: int32(app.Port)},
			IntervalSeconds:  healthIntervalSeconds,
			TimeoutSeconds:   healthTimeoutSeconds,
			FailureThreshold: healthFailureThreshold,
		}
	}
	if r := app.Resources; r.CPU != "" || r.Memory != "" {
		s.Resources = &v1alpha1.Resources{Cpu: r.CPU, Memory: r.Memory}
	}
	for _, v := range app.Volumes {
		s.Volumes = append(s.Volumes, &v1alpha1.VolumeMount{
			Name:      VolumeName(app.Name, v.Name),
			MountPath: v.MountPath,
		})
	}
	return s
}

// envVars lists the environment of the container: literal values in key
// order, then DATABASE_URL (a reference to the credential Secret), then one
// reference per secret key in the order they are written.
func envVars(app *App) []*v1alpha1.EnvVar {
	keys := make([]string, 0, len(app.Env))
	for k := range app.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []*v1alpha1.EnvVar
	for _, k := range keys {
		out = append(out, &v1alpha1.EnvVar{Name: k, Value: app.Env[k]})
	}
	if app.Database != (Database{}) {
		out = append(out, &v1alpha1.EnvVar{
			Name:      DatabaseURLEnv,
			ValueFrom: secretRef(DatabaseCredentialsName(app.Name), databaseCredentialsKey),
		})
	}
	for _, s := range app.Secrets {
		out = append(out, &v1alpha1.EnvVar{
			Name:      s,
			ValueFrom: secretRef(SecretsName(app.Name), s),
		})
	}
	return out
}

func secretRef(name, key string) *v1alpha1.EnvSource {
	return &v1alpha1.EnvSource{Secret: &v1alpha1.KeyRef{Name: name, Key: key}}
}

// inner returns the message inside an Object, for rendering.
func inner(o *Object) proto.Message {
	switch k := o.GetKind().(type) {
	case *v1alpha1.Object_Workload:
		return k.Workload
	case *v1alpha1.Object_Service:
		return k.Service
	case *v1alpha1.Object_Route:
		return k.Route
	case *v1alpha1.Object_Volume:
		return k.Volume
	case *v1alpha1.Object_Database:
		return k.Database
	}
	return nil
}
