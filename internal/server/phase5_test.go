package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/runtime"
)

const appYAML = `
kind: Secret
metadata: { name: db }
spec: { data: { password: hunter2 } }
---
kind: Config
metadata: { name: app }
spec: { data: { mode: prod, app.conf: "level=info" } }
---
kind: Workload
metadata: { name: api }
spec:
  image: example/api:1
  replicas: 2
  env:
    - { name: DB_PASSWORD, valueFrom: { secret: { name: db, key: password } } }
    - { name: MODE, valueFrom: { config: { name: app, key: mode } } }
    - { name: PLAIN, value: "yes" }
  files: [{ path: /etc/app, config: app }]
---
kind: Service
metadata: { name: api }
spec: { selector: api, ports: [{ port: 80, targetPort: 8080 }] }
`

// envOf returns a running container's environment as a map.
func envOf(t *testing.T, a *testAgent, id string) map[string]string {
	t.Helper()
	spec, ok := a.rt.Spec(id)
	if !ok {
		t.Fatalf("no container %s", id)
	}
	env := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func TestSecretsConfigsAndServices(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{}, fastControlPlane)
	admin := ts.admin(t)
	ctx := context.Background()
	a := ts.startAgent(t, admin, "node-1")
	eventually(t, "node ready", func() bool { return getNode(t, admin, "node-1").GetStatus().GetPhase() == NodeReady })
	applyYAML(t, admin, appYAML)
	eventually(t, "2 replicas running", func() bool { return len(a.running(t)) == 2 })

	// Env and files come from the Secret and the Config.
	id := a.running(t)[0]
	env := envOf(t, a, id)
	if env["DB_PASSWORD"] != "hunter2" || env["MODE"] != "prod" || env["PLAIN"] != "yes" {
		t.Fatalf("env = %v", env)
	}
	spec, _ := a.rt.Spec(id)
	if len(spec.Mounts) != 1 || spec.Mounts[0].Destination != "/etc/app" {
		t.Fatalf("mounts = %v", spec.Mounts)
	}
	file := filepath.Join(spec.Mounts[0].Source, "app.conf")
	if b, err := os.ReadFile(file); err != nil || string(b) != "level=info" {
		t.Fatalf("mounted file = %q, %v", b, err)
	}

	// The API never returns secret values.
	obj, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindSecret, Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := obj.GetSecret().GetSpec().GetData()["password"]; !ok || v != "" {
		t.Fatalf("secret via API = %v, want the key without its value", obj.GetSecret().GetSpec().GetData())
	}

	// The Service gets an IP and the replicas as endpoints, and reaches the
	// node's networking.
	eventually(t, "service with 2 endpoints on the node", func() bool {
		svcs, _ := a.net.state()
		return len(svcs) == 1 && svcs[0].ClusterIP == "10.96.0.1" && len(svcs[0].Endpoints) == 2 &&
			svcs[0].Ports[0].TargetPort == 8080
	})

	// A Config change reaches mounted files without recreating containers.
	before := a.running(t)
	applyYAML(t, admin, strings.Replace(appYAML, `level=info`, `level=debug`, 1))
	eventually(t, "file updated in place", func() bool {
		b, _ := os.ReadFile(file)
		return string(b) == "level=debug"
	})
	if after := a.running(t); !slices.Equal(after, before) {
		t.Fatalf("containers recreated by a config change: %v -> %v", before, after)
	}

	// A Secret change doesn't restart anything; a restart picks it up.
	applyYAML(t, admin, strings.Replace(appYAML, "hunter2", "correct-horse", 1))
	if env := envOf(t, a, id); env["DB_PASSWORD"] != "hunter2" {
		t.Fatalf("secret change restarted a container (env %v)", env)
	}
	if _, err := admin.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rolling restart with the new secret", func() bool {
		ids := a.running(t)
		if len(ids) != 2 {
			return false
		}
		for _, id := range ids {
			if envOf(t, a, id)["DB_PASSWORD"] != "correct-horse" {
				return false
			}
		}
		return true
	})

	// A reference to a missing Secret is reported, not guessed.
	applyYAML(t, admin, `
kind: Workload
metadata: { name: broken }
spec:
  image: example/api:1
  env: [{ name: X, valueFrom: { secret: { name: nope, key: x } } }]
`)
	eventually(t, "missing secret reported", func() bool {
		for _, r := range replicas(t, admin) {
			if r.GetSpec().GetWorkload() == "broken" && strings.Contains(r.GetStatus().GetMessage(), `secret "nope" not found`) {
				return true
			}
		}
		return false
	})
	cs, _ := a.rt.List(ctx)
	for _, c := range cs {
		if strings.HasPrefix(c.ID, "broken-") && c.State == runtime.StateRunning {
			t.Fatal("replica with a missing secret was started")
		}
	}
}

func TestPodCIDRAssignment(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	var cidrs []string
	for _, name := range []string{"n1", "n2", "n3"} {
		creds := filepath.Join(t.TempDir(), "c.conf")
		a, err := ts.newAgent(t, ts.token(t, admin, 1), name, creds)
		if err != nil {
			t.Fatal(err)
		}
		nc, err := a.NodeConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := a.NodeConfig(ctx)
		if again.GetPodCidr() != nc.GetPodCidr() {
			t.Fatalf("node %s got a different subnet the second time", name)
		}
		cidrs = append(cidrs, nc.GetPodCidr())
	}
	if want := []string{"10.250.1.0/24", "10.250.2.0/24", "10.250.3.0/24"}; !slices.Equal(cidrs, want) {
		t.Fatalf("subnets = %v, want %v", cidrs, want)
	}
}
