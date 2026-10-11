package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"example.com/shoal/api/v1alpha1"
)

// Racing reservations for one host and path prefix: exactly one wins, every
// other one is a conflict naming the winner (ADR 0015, pass E).
func TestRouteClaimExactlyOneWinsUnderConcurrency(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	ctx := context.Background()
	const racers = 16
	var (
		wins, losses atomic.Int32
		start        = make(chan struct{})
		wg           sync.WaitGroup
	)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			created, err := ts.reserveRoute(ctx, fmt.Sprintf("r%d", i), "race.example.com /")
			switch {
			case err == nil && created:
				wins.Add(1)
			case err != nil:
				losses.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != racers-1 {
		t.Fatalf("wins=%d losses=%d, want exactly one win and %d conflicts", wins.Load(), losses.Load(), racers-1)
	}
}

// The owner of a claim can reserve it again, and another Route cannot.
func TestRouteClaimOwnerAndConflict(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	ctx := context.Background()
	if created, err := ts.reserveRoute(ctx, "web", "app.example.com /"); err != nil || !created {
		t.Fatalf("first reserve: created=%v err=%v", created, err)
	}
	if created, err := ts.reserveRoute(ctx, "web", "app.example.com /"); err != nil || created {
		t.Fatalf("owner re-reserve: created=%v err=%v, want false and no error", created, err)
	}
	if _, err := ts.reserveRoute(ctx, "other", "app.example.com /"); err == nil {
		t.Fatal("a second Route got the same host and prefix")
	}
	// A different prefix on the same host is a different key.
	if created, err := ts.reserveRoute(ctx, "api", "app.example.com /api"); err != nil || !created {
		t.Fatalf("different prefix: created=%v err=%v", created, err)
	}
}

// Through the API: two Routes on one host and prefix applied concurrently,
// exactly one is accepted.
func TestConcurrentRouteAppliesExactlyOneAccepted(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	const racers = 8
	var (
		ok, conflicts atomic.Int32
		start         = make(chan struct{})
		wg            sync.WaitGroup
	)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, routeManifest(fmt.Sprintf("web%d", i), "shop.example.com", "/"))})
			switch {
			case err == nil:
				ok.Add(1)
			case status.Code(err) == codes.InvalidArgument:
				conflicts.Add(1)
			default:
				t.Errorf("apply %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || conflicts.Load() != racers-1 {
		t.Fatalf("accepted=%d conflicts=%d, want exactly one accepted", ok.Load(), conflicts.Load())
	}
	resp, err := admin.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindRoute})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetItems()); n != 1 {
		t.Fatalf("%d Routes stored, want 1", n)
	}
}

// Deleting a Route frees its host and prefix; moving it frees the old one.
func TestRouteClaimReleasedOnDeleteAndMove(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	ctx := context.Background()
	apply := func(y string) error {
		t.Helper()
		_, err := admin.Apply(ctx, &v1alpha1.ApplyRequest{Objects: decode(t, y)})
		return err
	}

	if err := apply(routeManifest("web", "a.example.com", "/")); err != nil {
		t.Fatal(err)
	}
	if err := apply(routeManifest("other", "a.example.com", "/")); err == nil {
		t.Fatal("conflict not detected while web holds the key")
	}

	// Move web to b.example.com: a.example.com is free again.
	if err := apply(routeManifest("web", "b.example.com", "/")); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := apply(routeManifest("other", "a.example.com", "/")); err != nil {
		t.Fatalf("old key was not released by the move: %v", err)
	}

	// Delete web: b.example.com is free.
	if _, err := admin.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindRoute, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := apply(routeManifest("third", "b.example.com", "/")); err != nil {
		t.Fatalf("deleted route did not release its key: %v", err)
	}
}

// A conditional release never frees a claim that another Route now holds.
func TestReleaseRouteOnlyFreesOwnClaim(t *testing.T) {
	ts := startServer(t, t.TempDir(), &clock{})
	ctx := context.Background()
	if _, err := ts.reserveRoute(ctx, "mine", "c.example.com /"); err != nil {
		t.Fatal(err)
	}
	ts.releaseRoute(ctx, "c.example.com /", "someone-else")
	if _, err := ts.reserveRoute(ctx, "theirs", "c.example.com /"); err == nil {
		t.Fatal("a release by a non-owner freed the claim")
	}
}
