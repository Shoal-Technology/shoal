package server

import (
	"context"
	"testing"
	"time"
)

func TestRunWhileLeader(t *testing.T) {
	leadership := make(chan bool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan context.Context, 4)
	finished := make(chan struct{}, 4)
	run := func(c context.Context) {
		started <- c
		<-c.Done()
		finished <- struct{}{}
	}
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runWhileLeader(ctx, leadership, run)
	}()

	const wait = 2 * time.Second
	send := func(v bool) {
		t.Helper()
		select {
		case leadership <- v:
		case <-time.After(wait):
			t.Fatalf("loop did not accept leadership=%v", v)
		}
	}
	startedWithin := func(what string) context.Context {
		t.Helper()
		select {
		case c := <-started:
			return c
		case <-time.After(wait):
			t.Fatalf("%s: run did not start", what)
			return nil
		}
	}
	finishedWithin := func(what string) {
		t.Helper()
		select {
		case <-finished:
		case <-time.After(wait):
			t.Fatalf("%s: run did not stop", what)
		}
	}
	noStart := func(what string) {
		t.Helper()
		select {
		case <-started:
			t.Fatalf("%s: run started", what)
		case <-time.After(50 * time.Millisecond):
		}
	}

	noStart("before any leadership")

	send(true)
	first := startedWithin("gaining leadership")

	send(true) // a repeated true must not start a second run
	noStart("repeated leadership")

	send(false)
	finishedWithin("losing leadership")
	if first.Err() == nil {
		t.Fatal("run context was not cancelled when leadership was lost")
	}

	send(true)
	second := startedWithin("regaining leadership")
	if second == first {
		t.Fatal("regained leadership reused the old run context")
	}

	cancel()
	select {
	case <-loopDone:
	case <-time.After(wait):
		t.Fatal("loop did not return after its context was cancelled")
	}
	finishedWithin("shutdown")
	if second.Err() == nil {
		t.Fatal("run context was not cancelled on shutdown")
	}
}
