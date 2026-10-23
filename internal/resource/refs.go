package resource

import (
	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Reference is a name that an object's spec uses to refer to another object
// of Kind (ADR 0023, pass B1). A reference resolves only within the project
// of the object that holds it.
type Reference struct {
	Kind string
	Name string
}

// References lists the references in spec, the spec of an object of kind.
// Empty names are left out. Kinds without references return nil.
func References(kind string, spec proto.Message) []Reference {
	var out []Reference
	add := func(k, n string) {
		if n != "" {
			out = append(out, Reference{Kind: k, Name: n})
		}
	}
	switch s := spec.(type) {
	case *v1alpha1.RouteSpec:
		add(v1alpha1.KindService, s.GetService())
		add(v1alpha1.KindSecret, s.GetTlsSecret())
	case *v1alpha1.ServiceSpec:
		add(v1alpha1.KindWorkload, s.GetSelector())
	case *v1alpha1.WorkloadSpec:
		for _, e := range s.GetEnv() {
			from := e.GetValueFrom()
			if ref := from.GetSecret(); ref != nil {
				add(v1alpha1.KindSecret, ref.GetName())
			}
			if ref := from.GetConfig(); ref != nil {
				add(v1alpha1.KindConfig, ref.GetName())
			}
		}
		for _, f := range s.GetFiles() {
			add(v1alpha1.KindSecret, f.GetSecret())
			add(v1alpha1.KindConfig, f.GetConfig())
		}
		for _, v := range s.GetVolumes() {
			add(v1alpha1.KindVolume, v.GetName())
		}
		for _, p := range s.GetImagePullSecrets() {
			add(v1alpha1.KindSecret, p)
		}
	case *v1alpha1.BuildConfigSpec:
		add(v1alpha1.KindSecret, s.GetCredentialsSecret())
		add(v1alpha1.KindWorkload, s.GetOnSuccess().GetUpdateWorkload())
	case *v1alpha1.PipelineSpec:
		add(v1alpha1.KindSecret, s.GetCredentialsSecret())
	case *v1alpha1.EnvironmentSpec:
		// The deploy targets of an environment (ADR 0022, section 10). A deploy
		// job may only set these, so they must be in the environment's project.
		for _, w := range s.GetWorkloads() {
			add(v1alpha1.KindWorkload, w)
		}
	}
	return out
}
