package resource

import (
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// TestInnerEveryKind checks that Inner returns the resource of every kind in the
// Object oneof. The CLI prints a get with Inner, so a kind it misses prints
// nothing (the pipeline kinds did, until this test).
func TestInnerEveryKind(t *testing.T) {
	oneof := (&v1alpha1.Object{}).ProtoReflect().Descriptor().Oneofs().ByName("kind")
	if oneof == nil {
		t.Fatal("Object has no oneof named kind")
	}
	fields := oneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		obj := &v1alpha1.Object{}
		m := obj.ProtoReflect()
		inner := m.NewField(fd)
		m.Set(fd, inner)
		if got := Inner(obj); got == nil {
			t.Errorf("Inner returns nil for %s", fd.Name())
		}
	}
}
