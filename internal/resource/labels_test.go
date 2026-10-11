package resource

import (
	"maps"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// TestObjectLabelsEveryKind sets labels on the metadata of every kind in the
// Object oneof and checks that ObjectLabels returns them. A kind missing from
// metadataOf loses its labels when it is applied, so the next deploy finds the
// object unlabelled and refuses to touch it (ADR 0017, E13). Database was
// missing until the e2e run deploy-e2e 38138904662 caught it.
func TestObjectLabelsEveryKind(t *testing.T) {
	want := map[string]string{"app": "blog", "shoal.dev/managed-by": "app"}
	oneof := (&v1alpha1.Object{}).ProtoReflect().Descriptor().Oneofs().ByName("kind")
	if oneof == nil {
		t.Fatal("Object has no oneof named kind")
	}
	fields := oneof.Fields()
	if fields.Len() == 0 {
		t.Fatal("Object.kind has no members")
	}
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		obj := &v1alpha1.Object{}
		m := obj.ProtoReflect()
		inner := m.NewField(fd).Message()
		md := inner.Descriptor().Fields().ByName("metadata")
		if md == nil {
			t.Fatalf("%s has no metadata field", fd.Name())
		}
		inner.Set(md, protoreflect.ValueOfMessage((&v1alpha1.ObjectMeta{Name: "x", Labels: want}).ProtoReflect()))
		m.Set(fd, protoreflect.ValueOfMessage(inner))
		if got := ObjectLabels(obj); !maps.Equal(got, want) {
			t.Errorf("%s: ObjectLabels = %v, want %v", fd.Name(), got, want)
		}
	}
}
