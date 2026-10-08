package store

import (
	"context"
	"sync"
)

// watchBuffer is the number of events a slow watcher can fall behind before
// events are dropped.
const watchBuffer = 64

// Hub fans events out to watchers. Store implementations embed it and call
// Publish after every committed write. The zero value is ready to use.
type Hub struct {
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

type subscription struct {
	kinds map[string]bool // empty = all
	ch    chan Event
}

// Subscribe implements Store.Watch.
func (h *Hub) Subscribe(ctx context.Context, kinds ...string) <-chan Event {
	s := &subscription{kinds: map[string]bool{}, ch: make(chan Event, watchBuffer)}
	for _, k := range kinds {
		s.kinds[k] = true
	}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[*subscription]struct{}{}
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, s)
		close(s.ch) // under the lock, so Publish never sends on a closed channel
	}()
	return s.ch
}

// Publish delivers e to every interested watcher without blocking.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if len(s.kinds) > 0 && !s.kinds[e.Kind] {
			continue
		}
		select {
		case s.ch <- e:
		default: // receiver is behind; it will catch up on its next resync
		}
	}
}
