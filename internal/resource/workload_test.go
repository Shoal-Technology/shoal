package resource

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// The example from the project brief.
const exampleWorkload = `
kind: Workload
metadata: { name: web }
spec:
  image: nginx:1.27
  replicas: 3
  ports: [{ containerPort: 80 }]
  resources: { cpu: 200m, memory: 128Mi }
  healthCheck: { http: { path: /, port: 80 }, intervalSeconds: 5 }
  update: { maxUnavailable: 1 }
`

func decodeWorkload(t *testing.T, y string) *v1alpha1.Workload {
	t.Helper()
	objs, err := Decode([]byte(y))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 1 || objs[0].GetWorkload() == nil {
		t.Fatalf("Decode = %v, want one Workload", objs)
	}
	return objs[0].GetWorkload()
}

func TestDecodeExample(t *testing.T) {
	w := decodeWorkload(t, exampleWorkload)
	DefaultWorkload(w)
	if err := ValidateWorkload(w); err != nil {
		t.Fatalf("example is invalid: %v", err)
	}
	s := w.Spec
	if w.Metadata.Name != "web" || s.Image != "nginx:1.27" || s.GetReplicas() != 3 ||
		s.Ports[0].ContainerPort != 80 || s.Ports[0].Protocol != "tcp" ||
		s.Resources.Cpu != "200m" || s.Resources.Memory != "128Mi" ||
		s.HealthCheck.Http.Path != "/" || s.HealthCheck.IntervalSeconds != 5 || s.HealthCheck.TimeoutSeconds != 2 ||
		s.RestartPolicy != "Always" || s.Update.GetMaxUnavailable() != 1 {
		t.Fatalf("unexpected decode: %v", w)
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    int
		wantErr string
	}{
		{name: "multiple documents and empty ones", want: 2, yaml: `
# leading comment
---
kind: Workload
metadata: {name: a}
spec: {image: busybox}
--- # trailing comment
---
apiVersion: v1alpha1
kind: Workload
metadata: {name: b}
spec: {image: busybox}
`},
		{name: "numeric quantities", want: 1, yaml: "kind: Workload\nmetadata: {name: a}\nspec: {image: x, resources: {cpu: 0.5, memory: 1024}}"},
		{name: "unknown field", wantErr: "unknown field", yaml: "kind: Workload\nmetadata: {name: a}\nspec: {image: x, replica: 3}"},
		{name: "wrong apiVersion", wantErr: "unsupported apiVersion", yaml: "apiVersion: v1\nkind: Workload\nmetadata: {name: a}"},
		{name: "missing kind", wantErr: "kind is required", yaml: "metadata: {name: a}"},
		{name: "unsupported kind", wantErr: "cannot be applied", yaml: "kind: Node\nmetadata: {name: a}"},
		{name: "empty", wantErr: "no objects", yaml: "# nothing\n"},
		{name: "bad yaml", wantErr: "document 1", yaml: "kind: [unclosed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs, err := Decode([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(objs) != tt.want {
				t.Fatalf("got %d objects, want %d", len(objs), tt.want)
			}
		})
	}
	w := decodeWorkload(t, "kind: Workload\nmetadata: {name: a}\nspec: {image: x, resources: {cpu: 0.5, memory: 1024}}")
	if w.Spec.Resources.Cpu != "0.5" || w.Spec.Resources.Memory != "1024" {
		t.Fatalf("numeric quantities = %v", w.Spec.Resources)
	}
}

func TestValidateWorkload(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(w *v1alpha1.Workload)
		wantErr string // empty = valid
	}{
		{name: "valid", mutate: func(*v1alpha1.Workload) {}},
		{name: "zero replicas allowed", mutate: func(w *v1alpha1.Workload) { w.Spec.Replicas = proto.Int32(0) }},
		{name: "missing name", wantErr: "metadata.name: required", mutate: func(w *v1alpha1.Workload) { w.Metadata.Name = "" }},
		{name: "uppercase name", wantErr: "metadata.name", mutate: func(w *v1alpha1.Workload) { w.Metadata.Name = "Web" }},
		{name: "name too long", wantErr: "metadata.name", mutate: func(w *v1alpha1.Workload) { w.Metadata.Name = strings.Repeat("a", 53) }},
		{name: "missing image", wantErr: "spec.image: required", mutate: func(w *v1alpha1.Workload) { w.Spec.Image = "" }},
		{name: "bad image", wantErr: "spec.image: invalid", mutate: func(w *v1alpha1.Workload) { w.Spec.Image = "Nginx:1.27" }},
		{name: "negative replicas", wantErr: "spec.replicas", mutate: func(w *v1alpha1.Workload) { w.Spec.Replicas = proto.Int32(-1) }},
		{name: "too many replicas", wantErr: "spec.replicas", mutate: func(w *v1alpha1.Workload) { w.Spec.Replicas = proto.Int32(1001) }},
		{name: "bad env name", wantErr: "spec.env[0].name", mutate: func(w *v1alpha1.Workload) {
			w.Spec.Env = []*v1alpha1.EnvVar{{Name: "1BAD"}}
		}},
		{name: "duplicate env", wantErr: "spec.env[1].name: duplicate", mutate: func(w *v1alpha1.Workload) {
			w.Spec.Env = []*v1alpha1.EnvVar{{Name: "A"}, {Name: "A"}}
		}},
		{name: "port out of range", wantErr: "spec.ports[0].containerPort", mutate: func(w *v1alpha1.Workload) { w.Spec.Ports[0].ContainerPort = 70000 }},
		{name: "duplicate host port", wantErr: "spec.ports[1].hostPort: duplicate", mutate: func(w *v1alpha1.Workload) {
			w.Spec.Ports = []*v1alpha1.Port{{ContainerPort: 80, HostPort: 8080, Protocol: "tcp"}, {ContainerPort: 81, HostPort: 8080, Protocol: "tcp"}}
		}},
		{name: "same host port different protocol", mutate: func(w *v1alpha1.Workload) {
			w.Spec.Ports = []*v1alpha1.Port{{ContainerPort: 53, HostPort: 53, Protocol: "tcp"}, {ContainerPort: 53, HostPort: 53, Protocol: "udp"}}
		}},
		{name: "bad protocol", wantErr: "spec.ports[0].protocol", mutate: func(w *v1alpha1.Workload) { w.Spec.Ports[0].Protocol = "sctp" }},
		{name: "bad cpu", wantErr: "spec.resources.cpu", mutate: func(w *v1alpha1.Workload) { w.Spec.Resources.Cpu = "lots" }},
		{name: "zero cpu", wantErr: "spec.resources.cpu: must be greater", mutate: func(w *v1alpha1.Workload) { w.Spec.Resources.Cpu = "0m" }},
		{name: "tiny memory", wantErr: "spec.resources.memory: must be at least", mutate: func(w *v1alpha1.Workload) { w.Spec.Resources.Memory = "1Ki" }},
		{name: "health check with both probes", wantErr: "exactly one", mutate: func(w *v1alpha1.Workload) {
			w.Spec.HealthCheck.Tcp = &v1alpha1.TCPHealthCheck{Port: 80}
		}},
		{name: "health check path", wantErr: "http.path", mutate: func(w *v1alpha1.Workload) { w.Spec.HealthCheck.Http.Path = "health" }},
		{name: "health check timeout above interval", wantErr: "timeoutSeconds", mutate: func(w *v1alpha1.Workload) {
			w.Spec.HealthCheck.TimeoutSeconds = 30
		}},
		{name: "bad restart policy", wantErr: "spec.restartPolicy", mutate: func(w *v1alpha1.Workload) { w.Spec.RestartPolicy = "Sometimes" }},
		{name: "maxUnavailable zero", wantErr: "spec.update.maxUnavailable", mutate: func(w *v1alpha1.Workload) {
			w.Spec.Update.MaxUnavailable = proto.Int32(0)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := decodeWorkload(t, exampleWorkload)
			DefaultWorkload(w)
			tt.mutate(w)
			err := ValidateWorkload(w)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultClearsServerFields(t *testing.T) {
	w := decodeWorkload(t, exampleWorkload)
	w.Metadata.Uid, w.Metadata.Generation = "x", 7
	w.Status = &v1alpha1.WorkloadStatus{}
	DefaultWorkload(w)
	if w.Metadata.Uid != "" || w.Metadata.Generation != 0 || w.Status != nil || w.Metadata.Name != "web" {
		t.Fatalf("server fields not cleared: %v", w)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	w := decodeWorkload(t, exampleWorkload)
	DefaultWorkload(w)
	y, err := EncodeYAML(w)
	if err != nil {
		t.Fatal(err)
	}
	again := decodeWorkload(t, string(y))
	DefaultWorkload(again)
	if !proto.Equal(w, again) {
		t.Fatalf("round trip changed the workload:\n%s", y)
	}
}
