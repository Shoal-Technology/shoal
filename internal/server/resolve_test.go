package server

import (
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

func TestResolvePullSecrets(t *testing.T) {
	good := `{"auths":{"ghcr.io":{"username":"bot","password":"tok"}}}`
	// Not JSON: the error must not echo the password that follows it.
	broken := `{"auths":{"ghcr.io":{"username":"bot","password":"tok"}}, "x"`
	d := &dataSources{
		secrets: map[string]map[string]string{
			"ghcr":   {resource.DockerConfigKey: good},
			"plain":  {"k": "v"},
			"broken": {resource.DockerConfigKey: broken},
		},
		secretTypes: map[string]string{
			"ghcr":   resource.SecretTypeDockerConfigJSON,
			"broken": resource.SecretTypeDockerConfigJSON,
			// "plain" has no type: it reads as Opaque.
		},
		configs: map[string]map[string]string{},
	}
	tests := []struct {
		name        string
		pull        []string
		wantProblem string // prefix; empty means resolved
		wantHosts   []string
	}{
		{"resolves a dockerconfigjson", []string{"ghcr"}, "", []string{"ghcr.io"}},
		{"missing secret", []string{"nope"}, `image pull secret "nope" not found`, nil},
		{"wrong type", []string{"plain"}, `image pull secret "plain" has type Opaque`, nil},
		{"does not parse", []string{"broken"}, `image pull secret "broken":`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &v1alpha1.Assignment{Template: &v1alpha1.WorkloadSpec{Image: "ghcr.io/acme/web:1", ImagePullSecrets: tc.pull}}
			d.resolve(a)
			if strings.Contains(a.GetProblem(), "tok") {
				t.Fatalf("problem leaks a credential: %q", a.GetProblem())
			}
			if tc.wantProblem == "" {
				if a.GetProblem() != "" {
					t.Fatalf("unexpected problem: %s", a.GetProblem())
				}
				var hosts []string
				for _, c := range a.GetRegistryCredentials() {
					hosts = append(hosts, c.GetHost())
					if c.GetUsername() != "bot" || c.GetPassword() != "tok" {
						t.Fatalf("credential = %v", c)
					}
				}
				if strings.Join(hosts, ",") != strings.Join(tc.wantHosts, ",") {
					t.Fatalf("hosts = %v, want %v", hosts, tc.wantHosts)
				}
				return
			}
			if !strings.HasPrefix(a.GetProblem(), tc.wantProblem) {
				t.Fatalf("problem = %q, want prefix %q", a.GetProblem(), tc.wantProblem)
			}
			if len(a.GetRegistryCredentials()) != 0 {
				t.Fatalf("a failed resolution still returned credentials: %v", a.GetRegistryCredentials())
			}
		})
	}
}
