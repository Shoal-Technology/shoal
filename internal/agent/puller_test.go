package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/fake"
)

func TestPullerPassesLoginsToRuntime(t *testing.T) {
	rt := fake.New(nil)
	p := newImagePuller(rt, time.Second, time.Second)
	defer p.close()
	const ref = "ghcr.io/acme/web:1"
	auth := []runtime.RegistryAuth{{Host: "ghcr.io", Username: "bot", Password: "tok"}}
	ready, err := p.ensure(context.Background(), ref, auth)
	if err != nil || !ready {
		t.Fatalf("ensure = %v, %v; want ready", ready, err)
	}
	if got := rt.PullAuth(ref); !reflect.DeepEqual(got, auth) {
		t.Fatalf("runtime pulled with %v, want %v", got, auth)
	}
}

func TestAssignmentFromAPICarriesPullLogins(t *testing.T) {
	a := &v1alpha1.Assignment{
		Replica:  "web-1",
		Template: &v1alpha1.WorkloadSpec{Image: "ghcr.io/acme/web:1", ImagePullSecrets: []string{"ghcr"}},
		RegistryCredentials: []*v1alpha1.RegistryCredential{
			{Host: "ghcr.io", Username: "bot", Password: "tok"},
		},
	}
	got, err := AssignmentFromAPI(a)
	if err != nil {
		t.Fatal(err)
	}
	want := []runtime.RegistryAuth{{Host: "ghcr.io", Username: "bot", Password: "tok"}}
	if !reflect.DeepEqual(got.PullAuth, want) {
		t.Fatalf("PullAuth = %v, want %v", got.PullAuth, want)
	}
}

func TestPullKeySeparatesLogins(t *testing.T) {
	const ref = "ghcr.io/acme/web:1"
	anon := pullKey(ref, nil)
	bot := pullKey(ref, []runtime.RegistryAuth{{Host: "ghcr.io", Username: "bot", Password: "a"}})
	other := pullKey(ref, []runtime.RegistryAuth{{Host: "ghcr.io", Username: "ci", Password: "a"}})
	if anon == bot || bot == other {
		t.Fatalf("pulls with different logins share a key: %q %q %q", anon, bot, other)
	}
	if bot != pullKey(ref, []runtime.RegistryAuth{{Host: "ghcr.io", Username: "bot", Password: "rotated"}}) {
		t.Fatalf("a rotated password changes the pull key")
	}
}
