package resource

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func validVolume() *v1alpha1.VolumeSpec {
	s := &v1alpha1.VolumeSpec{Size: "10Gi"}
	DefaultVolume(s)
	return s
}

func TestDefaultVolume(t *testing.T) {
	s := &v1alpha1.VolumeSpec{Size: "1Gi"}
	DefaultVolume(s)
	if s.AccessMode != "RWO" || s.Mode != "0700" || s.Retention != "Retain" || s.Owner == nil {
		t.Errorf("defaults = %+v", s)
	}
}

func TestValidateVolume(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*v1alpha1.VolumeSpec)
		obj  string // "" = valid; else a substring of the error
	}{
		{name: "valid", mut: func(*v1alpha1.VolumeSpec) {}},
		{name: "size missing", mut: func(s *v1alpha1.VolumeSpec) { s.Size = "" }, obj: "spec.size"},
		{name: "size not a quantity", mut: func(s *v1alpha1.VolumeSpec) { s.Size = "ten" }, obj: "spec.size"},
		{name: "size too small", mut: func(s *v1alpha1.VolumeSpec) { s.Size = "1Mi" }, obj: "at least 16Mi"},
		{name: "rwx rejected", mut: func(s *v1alpha1.VolumeSpec) { s.AccessMode = "RWX" }, obj: "only RWO"},
		{name: "negative owner", mut: func(s *v1alpha1.VolumeSpec) { s.Owner = &v1alpha1.VolumeOwner{Uid: -1} }, obj: "spec.owner"},
		{name: "mode not octal", mut: func(s *v1alpha1.VolumeSpec) { s.Mode = "0999" }, obj: "spec.mode"},
		{name: "retention unknown", mut: func(s *v1alpha1.VolumeSpec) { s.Retention = "Keep" }, obj: "Retain or Delete"},
		{name: "retention delete ok", mut: func(s *v1alpha1.VolumeSpec) { s.Retention = "Delete" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validVolume()
			c.mut(s)
			err := ValidateVolume("pg-data", s)
			switch {
			case c.obj == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.obj != "" && (err == nil || !strings.Contains(err.Error(), c.obj)):
				t.Fatalf("error = %v, want one containing %q", err, c.obj)
			}
		})
	}
	if err := ValidateVolume("Bad_Name", validVolume()); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestPrepareAcceptsVolumeObjects(t *testing.T) {
	obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Volume{Volume: &v1alpha1.Volume{
		Metadata: &v1alpha1.ObjectMeta{Name: "pg-data"},
		Spec:     &v1alpha1.VolumeSpec{Size: "20Gi"},
	}}}
	kind, name, spec, err := Prepare(obj)
	if err != nil {
		t.Fatal(err)
	}
	if kind != v1alpha1.KindVolume || name != "pg-data" {
		t.Errorf("kind/name = %s/%s", kind, name)
	}
	if s := spec.(*v1alpha1.VolumeSpec); s.GetRetention() != "Retain" || s.GetMode() != "0700" {
		t.Errorf("prepared spec not defaulted: %+v", s)
	}
}

func volumeWorkload(replicas int32, vols ...*v1alpha1.VolumeMount) *v1alpha1.Workload {
	w := &v1alpha1.Workload{
		Metadata: &v1alpha1.ObjectMeta{Name: "pg"},
		Spec: &v1alpha1.WorkloadSpec{
			Image: "postgres:16", Replicas: proto.Int32(replicas), Volumes: vols,
		},
	}
	DefaultWorkload(w)
	return w
}

// A read-write volume is data that only one writer may touch: replicas over
// one is rejected (ADR 0016, section 1.1).
func TestWorkloadRejectsReadWriteVolumeWithReplicasOverOne(t *testing.T) {
	w := volumeWorkload(2, &v1alpha1.VolumeMount{Name: "pg-data", MountPath: "/var/lib/postgresql/data"})
	err := ValidateWorkload(w)
	if err == nil || !strings.Contains(err.Error(), "replicas of at most 1") {
		t.Fatalf("error = %v, want replicas rejection", err)
	}
}

func TestWorkloadAllowsReadOnlyVolumeWithManyReplicas(t *testing.T) {
	w := volumeWorkload(3, &v1alpha1.VolumeMount{Name: "assets", MountPath: "/assets", ReadOnly: true})
	if err := ValidateWorkload(w); err != nil {
		t.Fatalf("read-only volume with 3 replicas: %v", err)
	}
}

func TestWorkloadVolumeMountRules(t *testing.T) {
	cases := []struct {
		name string
		vols []*v1alpha1.VolumeMount
		want string
	}{
		{"bad name", []*v1alpha1.VolumeMount{{Name: "Pg_data", MountPath: "/d"}}, "Volume name"},
		{"duplicate name", []*v1alpha1.VolumeMount{{Name: "d", MountPath: "/a"}, {Name: "d", MountPath: "/b"}}, "duplicate volume"},
		{"mount path not clean", []*v1alpha1.VolumeMount{{Name: "d", MountPath: "/a/../b"}}, "clean absolute"},
		{"mount path root", []*v1alpha1.VolumeMount{{Name: "d", MountPath: "/"}}, "clean absolute"},
		{"duplicate path", []*v1alpha1.VolumeMount{{Name: "a", MountPath: "/x"}, {Name: "b", MountPath: "/x"}}, "already used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := volumeWorkload(1, c.vols...)
			err := ValidateWorkload(w)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestWorkloadVolumeMountClashesWithFileMount(t *testing.T) {
	w := volumeWorkload(1, &v1alpha1.VolumeMount{Name: "d", MountPath: "/etc/app"})
	w.Spec.Files = []*v1alpha1.FileMount{{Path: "/etc/app", Config: "cfg"}}
	if err := ValidateWorkload(w); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error = %v, want a clash with the file mount", err)
	}
}
