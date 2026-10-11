// Package deploy runs the app commands of ADR 0017 (section 2.5): it plans
// the objects of a shoal.yaml against live state, applies them as one batch,
// prunes orphans, waits for readiness and reports status. The CLI is a thin
// wrapper; the logic runs against any v1alpha1.ControlClient.
package deploy

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/appspec"
	"github.com/kaiserern/shoal/internal/resource"
)

// applyOrder is the order in which an app's kinds are listed and applied:
// the data that the workloads use comes first.
var applyOrder = []string{
	v1alpha1.KindVolume,
	v1alpha1.KindDatabase,
	v1alpha1.KindSecret,
	v1alpha1.KindWorkload,
	v1alpha1.KindService,
	v1alpha1.KindRoute,
}

// isData reports whether a kind holds data. Prune and delete keep these
// unless the user asks for them.
func isData(kind string) bool {
	return kind == v1alpha1.KindVolume || kind == v1alpha1.KindDatabase || kind == v1alpha1.KindSecret
}

// Ref identifies an object by kind and name.
type Ref struct {
	Kind string
	Name string
}

// String prints the ref as kubectl does: workload/blog.
func (r Ref) String() string { return strings.ToLower(r.Kind) + "/" + r.Name }

// rank orders kinds by applyOrder; unknown kinds sort last.
func rank(kind string) int {
	if i := slices.Index(applyOrder, kind); i >= 0 {
		return i
	}
	return len(applyOrder)
}

// innerOf returns the typed message inside an Object (Workload, Route, ...).
func innerOf(obj *v1alpha1.Object) proto.Message {
	if obj == nil {
		return nil
	}
	r := obj.ProtoReflect()
	fd := r.WhichOneof(r.Descriptor().Oneofs().ByName("kind"))
	if fd == nil {
		return nil
	}
	return r.Get(fd).Message().Interface()
}

// fieldOf returns a set message field of m, or nil.
func fieldOf(m proto.Message, name string) proto.Message {
	if m == nil {
		return nil
	}
	r := m.ProtoReflect()
	fd := r.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || !r.Has(fd) {
		return nil
	}
	return r.Get(fd).Message().Interface()
}

// Describe returns the kind, ref, metadata and spec of an object. The kind is
// the message name, which is the kind constant (Workload, Route, ...).
func Describe(obj *v1alpha1.Object) (Ref, *v1alpha1.ObjectMeta, proto.Message) {
	in := innerOf(obj)
	if in == nil {
		return Ref{}, nil, nil
	}
	meta, _ := fieldOf(in, "metadata").(*v1alpha1.ObjectMeta)
	kind := string(in.ProtoReflect().Descriptor().Name())
	return Ref{Kind: kind, Name: meta.GetName()}, meta, fieldOf(in, "spec")
}

// Live is the server state of an app's kinds, keyed by ref.
type Live map[Ref]*v1alpha1.Object

// ListLive lists every object of the kinds an app uses.
func ListLive(ctx context.Context, ctl v1alpha1.ControlClient) (Live, error) {
	live := Live{}
	for _, kind := range applyOrder {
		resp, err := ctl.List(ctx, &v1alpha1.ListRequest{Kind: kind})
		if err != nil {
			return nil, err
		}
		for _, obj := range resp.GetItems() {
			ref, _, _ := Describe(obj)
			live[ref] = obj
		}
	}
	return live, nil
}

// AppObjects returns the live objects labelled app=<app>, in apply order.
func AppObjects(ctx context.Context, ctl v1alpha1.ControlClient, app string) ([]*v1alpha1.Object, error) {
	live, err := ListLive(ctx, ctl)
	if err != nil {
		return nil, err
	}
	var objs []*v1alpha1.Object
	for _, obj := range live {
		if labelsOf(obj)[appspec.LabelApp] == app && labelsOf(obj)[resource.OwnedByLabel] == "" {
			objs = append(objs, obj)
		}
	}
	SortObjects(objs)
	return objs, nil
}

// SortObjects orders objects by apply order, then by name.
func SortObjects(objs []*v1alpha1.Object) {
	slices.SortStableFunc(objs, func(a, b *v1alpha1.Object) int {
		ra, _, _ := Describe(a)
		rb, _, _ := Describe(b)
		if c := rank(ra.Kind) - rank(rb.Kind); c != 0 {
			return c
		}
		return strings.Compare(ra.Name, rb.Name)
	})
}

// get fetches one object; a missing object is (nil, nil).
func get(ctx context.Context, ctl v1alpha1.ControlClient, ref Ref) (*v1alpha1.Object, error) {
	obj, err := ctl.Get(ctx, &v1alpha1.GetRequest{Kind: ref.Kind, Name: ref.Name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
}

// labelsOf returns the labels of a live object.
func labelsOf(obj *v1alpha1.Object) map[string]string {
	_, meta, _ := Describe(obj)
	return meta.GetLabels()
}
