package resource

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// TestReferencesPerKind checks every reference kind of the object kinds that
// hold references (ADR 0023, pass B1).
func TestReferencesPerKind(t *testing.T) {
	k := func(kind, name string) Reference { return Reference{Kind: kind, Name: name} }
	cases := []struct {
		name string
		kind string
		spec proto.Message
		want []Reference
	}{
		{"route service and tls secret", v1alpha1.KindRoute,
			&v1alpha1.RouteSpec{Service: "api", TlsSecret: "cert"},
			[]Reference{k(v1alpha1.KindService, "api"), k(v1alpha1.KindSecret, "cert")}},
		{"service selector", v1alpha1.KindService,
			&v1alpha1.ServiceSpec{Selector: "web"},
			[]Reference{k(v1alpha1.KindWorkload, "web")}},
		{"workload env from secret and config", v1alpha1.KindWorkload,
			&v1alpha1.WorkloadSpec{Env: []*v1alpha1.EnvVar{
				{Name: "A", ValueFrom: &v1alpha1.EnvSource{Secret: &v1alpha1.KeyRef{Name: "creds", Key: "k"}}},
				{Name: "B", ValueFrom: &v1alpha1.EnvSource{Config: &v1alpha1.KeyRef{Name: "settings", Key: "k"}}},
				{Name: "C", Value: "literal"},
			}},
			[]Reference{k(v1alpha1.KindSecret, "creds"), k(v1alpha1.KindConfig, "settings")}},
		{"workload files", v1alpha1.KindWorkload,
			&v1alpha1.WorkloadSpec{Files: []*v1alpha1.FileMount{{Path: "/a", Secret: "creds"}, {Path: "/b", Config: "settings"}}},
			[]Reference{k(v1alpha1.KindSecret, "creds"), k(v1alpha1.KindConfig, "settings")}},
		{"workload volumes and pull secrets", v1alpha1.KindWorkload,
			&v1alpha1.WorkloadSpec{Volumes: []*v1alpha1.VolumeMount{{Name: "data", MountPath: "/d"}}, ImagePullSecrets: []string{"regcred"}},
			[]Reference{k(v1alpha1.KindVolume, "data"), k(v1alpha1.KindSecret, "regcred")}},
		{"build config secret and onSuccess workload", v1alpha1.KindBuildConfig,
			&v1alpha1.BuildConfigSpec{CredentialsSecret: "creds", OnSuccess: &v1alpha1.BuildOnSuccess{UpdateWorkload: "web"}},
			[]Reference{k(v1alpha1.KindSecret, "creds"), k(v1alpha1.KindWorkload, "web")}},
		{"pipeline secret", v1alpha1.KindPipeline,
			&v1alpha1.PipelineSpec{CredentialsSecret: "creds"},
			[]Reference{k(v1alpha1.KindSecret, "creds")}},
		{"no references", v1alpha1.KindWorkload,
			&v1alpha1.WorkloadSpec{Image: "nginx"}, nil},
		{"kind without references", v1alpha1.KindVolume,
			&v1alpha1.VolumeSpec{Size: "1Gi"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := References(tc.kind, tc.spec); !slices.Equal(got, tc.want) {
				t.Fatalf("References = %v, want %v", got, tc.want)
			}
		})
	}
}
