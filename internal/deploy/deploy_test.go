package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/appspec"
)

// fakeControl is an in-memory stand-in for the Control service. Apply stores
// the normalized objects the way the server does, so a re-deploy compares
// equal. Methods the deploy code does not use panic (nil embedded client).
type fakeControl struct {
	v1alpha1.ControlClient
	mu      sync.Mutex
	store   map[Ref]*v1alpha1.Object
	deleted []Ref
	applies int
}

func newFake() *fakeControl {
	return &fakeControl{store: map[Ref]*v1alpha1.Object{}}
}

func (f *fakeControl) List(_ context.Context, req *v1alpha1.ListRequest, _ ...grpc.CallOption) (*v1alpha1.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &v1alpha1.ListResponse{}
	for ref, obj := range f.store {
		if ref.Kind == req.GetKind() {
			resp.Items = append(resp.Items, proto.Clone(obj).(*v1alpha1.Object))
		}
	}
	return resp, nil
}

func (f *fakeControl) Get(_ context.Context, req *v1alpha1.GetRequest, _ ...grpc.CallOption) (*v1alpha1.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.store[Ref{Kind: req.GetKind(), Name: req.GetName()}]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return proto.Clone(obj).(*v1alpha1.Object), nil
}

func (f *fakeControl) Apply(_ context.Context, req *v1alpha1.ApplyRequest, _ ...grpc.CallOption) (*v1alpha1.ApplyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	resp := &v1alpha1.ApplyResponse{}
	for _, obj := range req.GetObjects() {
		c := proto.Clone(obj).(*v1alpha1.Object)
		ref, meta, _ := Describe(c)
		spec, err := normalizedSpec(c)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		// The server stores the spec that Prepare returns, not the one sent.
		setSpec(c, spec)
		cur, exists := f.store[ref]
		action := "created"
		switch {
		case exists && proto.Equal(spec, fieldOf(innerOf(cur), "spec")) && labelsEqual(labelsOf(cur), meta.GetLabels()):
			action = "unchanged"
		case exists:
			action = "configured"
			f.store[ref] = c
		default:
			f.store[ref] = c
		}
		resp.Results = append(resp.Results, &v1alpha1.ApplyResult{Kind: ref.Kind, Name: ref.Name, Action: action})
	}
	return resp, nil
}

// setSpec replaces the spec of obj with spec.
func setSpec(obj *v1alpha1.Object, spec proto.Message) {
	in := innerOf(obj).ProtoReflect()
	fd := in.Descriptor().Fields().ByName("spec")
	in.Set(fd, protoreflect.ValueOfMessage(proto.Clone(spec).ProtoReflect()))
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeControl) Delete(_ context.Context, req *v1alpha1.DeleteRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := Ref{Kind: req.GetKind(), Name: req.GetName()}
	if _, ok := f.store[ref]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	delete(f.store, ref)
	f.deleted = append(f.deleted, ref)
	return &v1alpha1.DeleteResponse{}, nil
}

// liveObj returns the stored object for ref, for tests that set statuses.
func (f *fakeControl) liveObj(t *testing.T, ref Ref) *v1alpha1.Object {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.store[ref]
	if !ok {
		t.Fatalf("%s not stored", ref)
	}
	return obj
}

// example loads examples/shoal.yaml, with optional text replacements, and
// expands it.
func example(t *testing.T, replace ...string) []*v1alpha1.Object {
	t.Helper()
	data, err := os.ReadFile("../../examples/shoal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for i := 0; i+1 < len(replace); i += 2 {
		if !strings.Contains(s, replace[i]) {
			t.Fatalf("example does not contain %q", replace[i])
		}
		s = strings.Replace(s, replace[i], replace[i+1], 1)
	}
	app, err := appspec.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	objs, err := appspec.Expand(app)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func deployOpts(objs []*v1alpha1.Object, out *bytes.Buffer) Options {
	return Options{App: "blog", Desired: objs, Out: out}
}

func TestDeployAppliesLabelledObjects(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	var out bytes.Buffer
	res, err := Run(ctx, f, deployOpts(example(t), &out))
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{
		{v1alpha1.KindWorkload, "blog"}, {v1alpha1.KindService, "blog"}, {v1alpha1.KindRoute, "blog"},
		{v1alpha1.KindDatabase, "blog-db"}, {v1alpha1.KindVolume, "blog-uploads"}, {v1alpha1.KindSecret, "blog-secrets"},
	}
	if len(f.store) != len(want) {
		t.Fatalf("stored %d objects, want %d", len(f.store), len(want))
	}
	for _, ref := range want {
		obj, ok := f.store[ref]
		if !ok {
			t.Fatalf("%s not applied", ref)
		}
		if got := labelsOf(obj)[appspec.LabelApp]; got != "blog" {
			t.Errorf("%s has app label %q, want blog", ref, got)
		}
	}
	for _, c := range res.Changes {
		if c.Action != "create" {
			t.Errorf("%s: action %q on a fresh deploy, want create", c.Ref, c.Action)
		}
	}
	if !strings.Contains(out.String(), "applied 6 objects (6 created, 0 configured)") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
	if n := len(f.store[Ref{v1alpha1.KindSecret, "blog-secrets"}].GetSecret().GetSpec().GetData()); n != 0 {
		t.Errorf("new secret has %d keys, want 0", n)
	}
}

func TestRedeployIsNoop(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := Run(ctx, f, deployOpts(example(t), &out))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Changes {
		if c.Action != "unchanged" {
			t.Errorf("%s: action %q on re-deploy, want unchanged (reason %q)", c.Ref, c.Action, c.Reason)
		}
	}
	if !strings.Contains(out.String(), "(0 created, 0 configured)") {
		t.Errorf("re-deploy changed something:\n%s", out.String())
	}
}

func TestDryRunDiff(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	// On an empty server every object is a create, and nothing is written.
	var out bytes.Buffer
	res, err := Run(ctx, f, Options{App: "blog", Desired: example(t), DryRun: true, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.store) != 0 || f.applies != 0 {
		t.Fatalf("dry run wrote %d objects in %d applies", len(f.store), f.applies)
	}
	if len(res.Changes) != 6 || res.Changes[0].Action != "create" {
		t.Fatalf("dry run plan = %+v", res.Changes)
	}

	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	// Changing the image updates only the workload.
	changed := example(t, "ghcr.io/example/blog:1.4.2", "ghcr.io/example/blog:1.4.3")
	out.Reset()
	res, err = Run(ctx, f, Options{App: "blog", Desired: changed, DryRun: true, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Changes {
		want := "unchanged"
		if c.Ref.Kind == v1alpha1.KindWorkload {
			want = "update"
		}
		if c.Action != want {
			t.Errorf("%s: action %q, want %q", c.Ref, c.Action, want)
		}
	}
	if !strings.Contains(out.String(), "update     workload/blog (spec changed)") {
		t.Errorf("diff line missing:\n%s", out.String())
	}
	if img := f.store[Ref{v1alpha1.KindWorkload, "blog"}].GetWorkload().GetSpec().GetImage(); img != "ghcr.io/example/blog:1.4.2" {
		t.Errorf("dry run changed the stored image to %q", img)
	}
}

func TestPruneRemovesDroppedRouteKeepsData(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	// Dropping the domain, the database and the volume drops the Route, the
	// Database and the Volume from the manifest.
	noDomain := example(t,
		"domain: blog.example.com\n", "",
		"database: postgres\n", "",
		"volumes:\n  - name: uploads\n    mountPath: /app/uploads\n    size: 5Gi\n", "")
	var out bytes.Buffer
	opts := Options{App: "blog", Desired: noDomain, Prune: true, Out: &out}
	res, err := Run(ctx, f, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != (Ref{v1alpha1.KindRoute, "blog"}) {
		t.Fatalf("deleted = %v, want only route/blog", res.Deleted)
	}
	if _, ok := f.store[Ref{v1alpha1.KindRoute, "blog"}]; ok {
		t.Error("route/blog still stored after prune")
	}
	for _, ref := range []Ref{{v1alpha1.KindVolume, "blog-uploads"}, {v1alpha1.KindDatabase, "blog-db"}} {
		if _, ok := f.store[ref]; !ok {
			t.Errorf("%s was pruned without --prune-data", ref)
		}
	}
	if len(res.Kept) != 2 {
		t.Errorf("kept = %v, want the volume and the database", res.Kept)
	}
	if !strings.Contains(out.String(), "delete     route/blog") {
		t.Errorf("plan does not show the prune:\n%s", out.String())
	}
}

func TestPruneWithoutFlagOnlyReports(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	res, err := Run(ctx, f, deployOpts(example(t, "domain: blog.example.com\n", ""), new(bytes.Buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 0 || len(res.Orphans) != 1 {
		t.Fatalf("without --prune: deleted %v, orphans %v", res.Deleted, res.Orphans)
	}
	if _, ok := f.store[Ref{v1alpha1.KindRoute, "blog"}]; !ok {
		t.Error("route removed without --prune")
	}
}

func TestPlanRefusesObjectOfAnotherApp(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.store[Ref{v1alpha1.KindRoute, "blog"}] = &v1alpha1.Object{Kind: &v1alpha1.Object_Route{Route: &v1alpha1.Route{
		Metadata: &v1alpha1.ObjectMeta{Name: "blog", Labels: map[string]string{appspec.LabelApp: "other"}},
	}}}
	_, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer)))
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestWaitTimeoutNamesStuckObject(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	db := f.liveObj(t, Ref{v1alpha1.KindDatabase, "blog-db"})
	db.GetDatabase().Status = &v1alpha1.DatabaseStatus{Phase: "Provisioning", Message: "initdb running"}

	wctx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := Wait(wctx, f, WaitTargets(example(t)), 5*time.Millisecond, &out)
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want *NotReadyError", err)
	}
	msg := err.Error()
	for _, want := range []string{"workload/blog: 0/1 replicas ready", "database/blog-db: phase Provisioning: initdb running"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestWaitReturnsWhenReady(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	f.liveObj(t, Ref{v1alpha1.KindWorkload, "blog"}).GetWorkload().Status = &v1alpha1.WorkloadStatus{ReadyReplicas: 1}
	f.liveObj(t, Ref{v1alpha1.KindDatabase, "blog-db"}).GetDatabase().Status = &v1alpha1.DatabaseStatus{Phase: "Ready"}
	f.liveObj(t, Ref{v1alpha1.KindRoute, "blog"}).GetRoute().Status = &v1alpha1.RouteStatus{State: "Ready", CertState: "CertIssued"}

	wctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := Wait(wctx, f, WaitTargets(example(t)), time.Millisecond, &out); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !strings.Contains(out.String(), "ready      workload/blog") {
		t.Errorf("no ready line:\n%s", out.String())
	}
}

func TestAppStatusListsObjects(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	objs, err := AppObjects(ctx, f, "blog")
	if err != nil {
		t.Fatal(err)
	}
	rows := StatusRows(objs)
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Kind == v1alpha1.KindSecret && (r.State != "empty") {
			t.Errorf("secret row state %q, want empty", r.State)
		}
	}
	if got := URLFor("blog", objs); got != "https://blog.example.com" {
		t.Errorf("URL = %q", got)
	}
}

func TestAppDeleteKeepsDataByDefault(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	plan, err := DeleteApp(ctx, f, "blog", DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range f.deleted {
		if isData(ref.Kind) {
			t.Errorf("%s deleted without --data", ref)
		}
	}
	if len(plan.Keep) != 3 {
		t.Errorf("kept %v, want volume, database and secret", plan.Keep)
	}
	for _, ref := range []Ref{{v1alpha1.KindVolume, "blog-uploads"}, {v1alpha1.KindDatabase, "blog-db"}, {v1alpha1.KindSecret, "blog-secrets"}} {
		if _, ok := f.store[ref]; !ok {
			t.Errorf("%s gone", ref)
		}
	}
	if _, ok := f.store[Ref{v1alpha1.KindWorkload, "blog"}]; ok {
		t.Error("workload still stored")
	}

	// A declined confirmation deletes nothing.
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	_, err = DeleteApp(ctx, f, "blog", DeleteOptions{Data: true, Confirm: func(DeletePlan) bool { return false }})
	if !errors.Is(err, ErrAborted) || len(f.store) != 6 {
		t.Fatalf("declined delete: err %v, %d stored", err, len(f.store))
	}
}

func TestSecretSetDoesNotPrintValues(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	values, err := ParseAssignments([]string{"SESSION_KEY=s3cret-value", "TOKEN=-"}, strings.NewReader("tok-123\n"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := SetSecret(ctx, f, "blog", values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "SESSION_KEY,TOKEN" {
		t.Errorf("keys = %v", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "s3cret") || strings.Contains(k, "tok-123") {
			t.Errorf("key name %q holds a value", k)
		}
	}
	data := f.store[Ref{v1alpha1.KindSecret, "blog-secrets"}].GetSecret().GetSpec().GetData()
	if data["SESSION_KEY"] != "s3cret-value" || data["TOKEN"] != "tok-123" {
		t.Errorf("stored data = %v", data)
	}

	// A later deploy keeps the values.
	if _, err := Run(ctx, f, deployOpts(example(t), new(bytes.Buffer))); err != nil {
		t.Fatal(err)
	}
	data = f.store[Ref{v1alpha1.KindSecret, "blog-secrets"}].GetSecret().GetSpec().GetData()
	if data["SESSION_KEY"] != "s3cret-value" {
		t.Errorf("deploy overwrote the secret: %v", data)
	}

	_, err = ParseAssignments([]string{"not a pair"}, nil)
	if err == nil || strings.Contains(err.Error(), "not a pair") {
		t.Errorf("parse error = %v; it must not echo the argument", err)
	}
}

func TestSecretValueNotEchoedOnBadKey(t *testing.T) {
	_, err := ParseAssignments([]string{"1BAD=hunter2"}, nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v", err)
	}
}
