package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// routeManifest is one Route on the given host and path prefix.
func routeManifest(name, host, prefix string) string {
	return fmt.Sprintf(`kind: Route
metadata:
  name: %s
spec:
  host: "%s"
  pathPrefix: "%s"
  service: web
  port: 80
`, name, host, prefix)
}

// requireStatus fails unless err has the gRPC code and mentions msg.
func requireStatus(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(err.Error(), msg) {
		t.Fatalf("err = %v, want code %v containing %q", err, code, msg)
	}
}

func TestRouteApplyGetListDelete(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()

	apply := func(y string) error {
		t.Helper()
		_, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, y)})
		return err
	}
	action := func(y string) string {
		t.Helper()
		resp, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, y)})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		return resp.GetResults()[0].GetAction()
	}
	count := func() int {
		t.Helper()
		resp, err := admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindRoute})
		if err != nil {
			t.Fatal(err)
		}
		return len(resp.GetItems())
	}

	web := routeManifest("web", "app.example.com", "/")
	if got := action(web); got != "created" {
		t.Fatalf("first apply = %q, want created", got)
	}
	if got := action(web); got != "unchanged" {
		t.Fatalf("second apply = %q, want unchanged", got)
	}

	obj, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindRoute, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	spec := obj.GetRoute().GetSpec()
	if spec.GetHost() != "app.example.com" || spec.GetPathPrefix() != "/" ||
		spec.GetTls() != "auto" || !spec.GetRedirectHttp() {
		t.Fatalf("stored spec = %v", spec)
	}
	if n := count(); n != 1 {
		t.Fatalf("list = %d routes, want 1", n)
	}

	// Same host and path under another name: rejected.
	requireStatus(t, apply(routeManifest("other", "app.example.com", "/")),
		codes.InvalidArgument, `already used by route "web"`)

	// Same host, other path prefix: allowed.
	if got := action(routeManifest("api", "app.example.com", "/api")); got != "created" {
		t.Fatalf("api apply = %q, want created", got)
	}

	// Two new Routes that clash inside one batch: the whole batch is refused.
	requireStatus(t, apply(routeManifest("left", "b.example.com", "/")+"---\n"+routeManifest("right", "b.example.com", "/")),
		codes.InvalidArgument, `already used by route "left"`)
	if n := count(); n != 2 {
		t.Fatalf("after refused batch, list = %d routes, want 2", n)
	}

	// Invalid Routes are refused before anything is written.
	requireStatus(t, apply(routeManifest("w", "*.example.com", "/")), codes.InvalidArgument, "wildcard hosts are not supported")

	// Deleting a Route frees its host and path.
	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindRoute, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindRoute, Name: "web"}); status.Code(err) != codes.NotFound {
		t.Fatalf("get after delete: err = %v, want NotFound", err)
	}
	if got := action(routeManifest("other", "app.example.com", "/")); got != "created" {
		t.Fatalf("apply into freed host and path = %q, want created", got)
	}
	if n := count(); n != 2 {
		t.Fatalf("final list = %d routes, want 2", n)
	}
}
