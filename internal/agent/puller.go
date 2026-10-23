package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// imagePuller pulls images in the background, so a slow pull doesn't hold up
// the reconcile pass and with it every other replica on the node.
type imagePuller struct {
	rt      runtime.Runtime
	timeout time.Duration // per pull
	wait    time.Duration // how long ensure waits for a pull it just started

	ctx    context.Context // cancelled by close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	pulls map[string]*pull // by pullKey: image reference and logins
}

type pull struct {
	done chan struct{}
	err  error // valid after done is closed
}

func newImagePuller(rt runtime.Runtime, timeout, wait time.Duration) *imagePuller {
	ctx, cancel := context.WithCancel(context.Background())
	return &imagePuller{rt: rt, timeout: timeout, wait: wait, ctx: ctx, cancel: cancel, pulls: map[string]*pull{}}
}

// ensure reports whether ref is available locally. If no pull of ref is in
// progress it starts one and waits up to p.wait for it, so local images and
// fast pulls are ready in the same pass. A finished pull's result is
// returned once; after a failure the next call starts a new pull. auth are
// the logins the pull uses. Only pulls of the same ref with the same logins
// are shared.
func (p *imagePuller) ensure(ctx context.Context, ref string, auth []runtime.RegistryAuth) (bool, error) {
	key := pullKey(ref, auth)
	p.mu.Lock()
	pl, inProgress := p.pulls[key]
	if !inProgress {
		pl = &pull{done: make(chan struct{})}
		p.pulls[key] = pl
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			pctx, cancel := context.WithTimeout(p.ctx, p.timeout)
			defer cancel()
			pl.err = p.rt.EnsureImage(pctx, ref, auth)
			close(pl.done)
		}()
	}
	p.mu.Unlock()

	if !inProgress {
		t := time.NewTimer(p.wait)
		defer t.Stop()
		select {
		case <-pl.done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	select {
	case <-pl.done:
		p.mu.Lock()
		if p.pulls[key] == pl {
			delete(p.pulls, key)
		}
		p.mu.Unlock()
		return pl.err == nil, pl.err
	default:
		return false, nil
	}
}

// pullKey identifies a pull by its image and the registry users it logs in
// as. Passwords are left out of the key.
func pullKey(ref string, auth []runtime.RegistryAuth) string {
	var b strings.Builder
	b.WriteString(ref)
	for _, a := range auth {
		b.WriteString("\x00" + a.Host + "\x00" + a.Username)
	}
	return b.String()
}

// close cancels pulls in progress and waits for them to return.
func (p *imagePuller) close() {
	p.cancel()
	p.wg.Wait()
}
