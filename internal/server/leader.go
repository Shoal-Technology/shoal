package server

import "context"

// alwaysLeader is the leadership signal for the sqlite store: one node,
// always the leader. The channel is never closed.
func alwaysLeader() <-chan bool {
	ch := make(chan bool, 1)
	ch <- true
	return ch
}

// runWhileLeader calls run with a fresh context each time leadership becomes
// true. It cancels that context as soon as leadership becomes false and waits
// for run to return before it does anything else. It returns when ctx is
// done or leadership is closed, after stopping any running run.
func runWhileLeader(ctx context.Context, leadership <-chan bool, run func(context.Context)) {
	// start launches run and returns the function that stops it.
	start := func() (halt func()) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			run(runCtx)
		}()
		return func() {
			cancel()
			<-done
		}
	}
	var halt func() // stops the current run; nil when this node is not leader
	defer func() {
		if halt != nil {
			halt()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case leader, ok := <-leadership:
			if !ok {
				return
			}
			switch {
			case leader && halt == nil:
				halt = start()
			case !leader && halt != nil:
				halt()
				halt = nil
			}
		}
	}
}
