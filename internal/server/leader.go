package server

import (
	"context"
	"sort"
	"sync"
)

// leaderGate is a test hook. It tracks which nodes are running the leader
// tasks and counts each moment when more than one node runs them.
type leaderGate struct {
	mu         sync.Mutex
	running    map[string]bool
	violations int
}

func newLeaderGate() *leaderGate {
	return &leaderGate{running: map[string]bool{}}
}

func (g *leaderGate) enter(node string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running[node] = true
	if len(g.running) > 1 {
		g.violations++
	}
}

func (g *leaderGate) leave(node string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.running, node)
}

// snapshot returns the nodes running the leader tasks, sorted, and the number
// of overlaps seen so far.
func (g *leaderGate) snapshot() ([]string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var nodes []string
	for n := range g.running {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes, g.violations
}

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
