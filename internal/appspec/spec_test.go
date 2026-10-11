package appspec

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	k8syaml "sigs.k8s.io/yaml"

	"example.com/shoal/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// render writes objects as a multi-document YAML stream. Each object is
// shown without its oneof wrapper, and protojson with JSONToYAML sorts the
// keys, so the output is stable.
func render(t *testing.T, objs []*Object) []byte {
	t.Helper()
	var b bytes.Buffer
	for i, o := range objs {
		if i > 0 {
			b.WriteString("---\n")
		}
		j, err := protojson.Marshal(inner(o))
		if err != nil {
			t.Fatalf("protojson: %v", err)
		}
		y, err := k8syaml.JSONToYAML(j)
		if err != nil {
			t.Fatalf("yaml: %v", err)
		}
		b.Write(y)
	}
	return b.Bytes()
}

// inputFiles returns the files that match pattern, skipping the golden
// outputs.
func inputFiles(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".expanded.yaml") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no inputs match %s", pattern)
	}
	return out
}

func compareGolden(t *testing.T, golden string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/appspec -update", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; run go test ./internal/appspec -update\n--- got ---\n%s", golden, got)
	}
}

func TestGoldenExpansion(t *testing.T) {
	for _, f := range inputFiles(t, "testdata/*.yaml") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			app, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			objs, err := Expand(app)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			compareGolden(t, strings.TrimSuffix(f, ".yaml")+".expanded.yaml", render(t, objs))
		})
	}
}

func TestGoldenInvalid(t *testing.T) {
	for _, f := range inputFiles(t, "testdata/invalid/*.yaml") {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data)
			if err == nil {
				t.Fatal("Parse accepted an invalid spec")
			}
			compareGolden(t, strings.TrimSuffix(f, ".yaml")+".error.txt", []byte(err.Error()+"\n"))
		})
	}
}

func TestExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../examples/shoal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("examples/shoal.yaml: %v", err)
	}
}

func TestValidationTable(t *testing.T) {
	const web = "name: web\nimage: nginx:1.27\nport: 80\n"
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring of the error; "" means valid
	}{
		{"minimal", web, ""},
		{"unknown key names its line", "name: web\nimage: nginx\nimge: x\n", "line 3: field imge not found"},
		{"build is rejected", web + "build: .\n", "build: build is not supported"},
		{"name required", "image: nginx\n", "name: required"},
		{"name not a DNS label", "name: Web_App\nimage: nginx\n", "name: must be a lowercase DNS label"},
		{"name too long", "name: " + strings.Repeat("a", MaxAppNameLen+1) + "\nimage: nginx\n", "name: must be a lowercase DNS label"},
		{"image required", "name: web\nport: 80\n", "image: required"},
		{"bad image reference", "name: web\nimage: NGINX::bad\nport: 80\n", "spec.image"},
		{"port out of range", "name: web\nimage: nginx\nport: 70000\n", "port: must be between 1 and 65535"},
		{"domain needs port", "name: web\nimage: nginx\ndomain: web.example.com\n", "domain: needs port"},
		{"bad host", "name: web\nimage: nginx\nport: 80\ndomain: 10.0.0.1\n", "spec.host"},
		{"health needs port", "name: web\nimage: nginx\nhealth:\n  path: /healthz\n", "health: needs port"},
		{"health path must be absolute", "name: web\nimage: nginx\nport: 80\nhealth:\n  path: healthz\n", "health.path: must start with /"},
		{"replicas negative", "name: web\nimage: nginx\nport: 80\nreplicas: -1\n", "replicas: must be between 0 and"},
		{"replicas zero is allowed", "name: web\nimage: nginx\nport: 80\nreplicas: 0\n", ""},
		{"volume with two replicas", "name: web\nimage: nginx\nreplicas: 2\nvolumes:\n  - name: up\n    mountPath: /data\n    size: 1Gi\n", "replicas: must be 1 when volumes are set"},
		{"volume size required", "name: web\nimage: nginx\nvolumes:\n  - name: up\n    mountPath: /data\n", "volumes[0].size: required"},
		{"volume duplicate name", "name: web\nimage: nginx\nvolumes:\n  - name: up\n    mountPath: /a\n    size: 1Gi\n  - name: up\n    mountPath: /b\n    size: 1Gi\n", "volumes[1].name: duplicate"},
		{"volume mount path relative", "name: web\nimage: nginx\nvolumes:\n  - name: up\n    mountPath: data\n    size: 1Gi\n", "mountPath"},
		{"volume too small", "name: web\nimage: nginx\nvolumes:\n  - name: up\n    mountPath: /data\n    size: 1Mi\n", "size"},
		{"env name invalid", "name: web\nimage: nginx\nenv:\n  1BAD: x\n", "env.1BAD: invalid environment variable name"},
		{"secret key invalid", "name: web\nimage: nginx\nsecrets:\n  - bad-key\n", "secrets[0]: invalid key"},
		{"secret duplicate", "name: web\nimage: nginx\nsecrets:\n  - KEY\n  - KEY\n", "secrets[1]: duplicate"},
		{"secret also in env", "name: web\nimage: nginx\nenv:\n  KEY: v\nsecrets:\n  - KEY\n", "secrets[0]: \"KEY\" is also set in env"},
		{"database unknown engine", "name: web\nimage: nginx\ndatabase: mysql\n", "database.engine: must be postgres"},
		{"database size only", "name: web\nimage: nginx\ndatabase:\n  size: 5Gi\n", "database.engine: required"},
		{"database bad version", "name: web\nimage: nginx\ndatabase:\n  engine: postgres\n  version: 16.2-rc\n", "database.version"},
		{"database bad size", "name: web\nimage: nginx\ndatabase:\n  engine: postgres\n  size: lots\n", "database.size"},
		{"database short form ok", "name: web\nimage: nginx\ndatabase: postgres\n", ""},
		{"DATABASE_URL conflicts with database", "name: web\nimage: nginx\ndatabase: postgres\nenv:\n  DATABASE_URL: x\n", "env.DATABASE_URL: DATABASE_URL is set by database"},
		{"CPU zero", "name: web\nimage: nginx\nresources:\n  cpu: 0\n", "spec.resources.cpu"},
		{"memory too small", "name: web\nimage: nginx\nresources:\n  memory: 1Ki\n", "spec.resources.memory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := Parse([]byte(tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if _, err := Expand(app); err != nil {
					t.Fatalf("Expand: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted; want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestEveryObjectIsLabelled(t *testing.T) {
	data, err := os.ReadFile("testdata/web_db_domain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Expand(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) == 0 {
		t.Fatal("no objects")
	}
	for _, o := range objs {
		labels := labelsOf(o)
		if labels[LabelApp] != "blog" {
			t.Errorf("object %v: label app = %q, want blog", o.GetKind(), labels[LabelApp])
		}
	}
}

// labelsOf returns the metadata labels of an object.
func labelsOf(o *Object) map[string]string {
	switch k := o.GetKind().(type) {
	case *v1alpha1.Object_Workload:
		return k.Workload.GetMetadata().GetLabels()
	case *v1alpha1.Object_Service:
		return k.Service.GetMetadata().GetLabels()
	case *v1alpha1.Object_Route:
		return k.Route.GetMetadata().GetLabels()
	case *v1alpha1.Object_Volume:
		return k.Volume.GetMetadata().GetLabels()
	}
	return nil
}

// TestDatabaseReference checks the database case. The Database object is a
// TODO until the kind lands (ADR 0016), so only the env reference and the
// absence of a Database object are asserted.
func TestDatabaseReference(t *testing.T) {
	data, err := os.ReadFile("testdata/web_db_domain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	app, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Expand(app)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range objs {
		w := o.GetWorkload()
		if w == nil {
			continue
		}
		for _, e := range w.GetSpec().GetEnv() {
			if e.GetName() != DatabaseURLEnv {
				continue
			}
			found = true
			ref := e.GetValueFrom().GetSecret()
			if ref.GetName() != "blog-db-credentials" || ref.GetKey() != "url" {
				t.Errorf("DATABASE_URL secret ref = %v, want blog-db-credentials/url", ref)
			}
			if e.GetValue() != "" {
				t.Errorf("DATABASE_URL has a plain value; it must only reference the Secret")
			}
		}
	}
	if !found {
		t.Fatal("no DATABASE_URL env on the Workload")
	}
	if n := len(objs); n != 3 {
		t.Errorf("got %d objects, want 3 (Workload, Service, Route); the Database is a TODO", n)
	}
}

func TestExpandIsDeterministic(t *testing.T) {
	for _, f := range inputFiles(t, "testdata/*.yaml") {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var first []byte
		for i := 0; i < 3; i++ {
			app, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			objs, err := Expand(app)
			if err != nil {
				t.Fatal(err)
			}
			got := render(t, objs)
			if i == 0 {
				first = got
			} else if !bytes.Equal(first, got) {
				t.Fatalf("%s: expansion differs between runs", f)
			}
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("name: web\nimage: nginx\nport: 80\ndomain: web.example.com\n"))
	f.Add([]byte("name: w\nimage: x\ndatabase: postgres\nsecrets: [K]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		app, err := Parse(data)
		if err != nil {
			return
		}
		// Parse validates, so an accepted spec must also expand.
		if _, err := Expand(app); err != nil {
			t.Fatalf("Parse accepted a spec that Expand rejects: %v", err)
		}
	})
}
