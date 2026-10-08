package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// HealthCheck is a replica's health probe, run by the agent against the
// container's IP.
type HealthCheck struct {
	HTTPPath         string // HTTP GET if set, else a TCP connect
	Port             int
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	InitialDelay     time.Duration
}

// probeTarget identifies one run of a container: a new start (or IP, or
// check) gets a fresh probe with a clean failure count.
type probeTarget struct {
	ip        string
	startedAt time.Time
	check     HealthCheck
}

// health is the result of probing a replica.
type health struct {
	ready     bool   // passed at least once and not failed failureThreshold times since
	unhealthy bool   // failed failureThreshold times in a row: restart it
	message   string // why it isn't ready
}

// prober runs one goroutine per probed replica. Reconcile passes tell it
// what to probe (sync) and read the latest results (result).
type prober struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	check  func(ctx context.Context, ip string, hc HealthCheck) error

	mu     sync.Mutex
	probes map[string]*probe // by replica ID
}

type probe struct {
	target   probeTarget
	stop     context.CancelFunc
	health   health
	failures int
}

func newProber() *prober {
	ctx, cancel := context.WithCancel(context.Background())
	return &prober{ctx: ctx, cancel: cancel, check: runCheck, probes: map[string]*probe{}}
}

// result returns the health of the replica's current run. Before the
// first probe (or after a new start) the replica is not ready yet.
func (p *prober) result(id string, t probeTarget) health {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.probes[id]
	if !ok || pr.target != t {
		return health{message: "waiting for the first health check"}
	}
	return pr.health
}

// sync starts probes for targets that have none (or a different one) and
// stops probes of replicas no longer listed.
func (p *prober) sync(targets map[string]probeTarget) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, pr := range p.probes {
		if t, ok := targets[id]; !ok || t != pr.target {
			pr.stop()
			delete(p.probes, id)
		}
	}
	for id, t := range targets {
		if _, ok := p.probes[id]; ok {
			continue
		}
		ctx, stop := context.WithCancel(p.ctx)
		pr := &probe{target: t, stop: stop, health: health{message: "waiting for the first health check"}}
		p.probes[id] = pr
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.run(ctx, pr)
		}()
	}
}

// forget drops the probe of a replica that is being restarted.
func (p *prober) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.probes[id]; ok {
		pr.stop()
		delete(p.probes, id)
	}
}

func (p *prober) run(ctx context.Context, pr *probe) {
	hc := pr.target.check
	if err := sleep(ctx, time.Until(pr.target.startedAt.Add(hc.InitialDelay))); err != nil {
		return
	}
	tick := time.NewTicker(hc.Interval)
	defer tick.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, hc.Timeout)
		err := p.check(cctx, pr.target.ip, hc)
		cancel()
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		if err == nil {
			pr.failures = 0
			pr.health = health{ready: true}
		} else {
			pr.failures++
			pr.health.message = fmt.Sprintf("health check failed (%d/%d): %v", pr.failures, hc.FailureThreshold, err)
			if pr.failures >= hc.FailureThreshold {
				pr.health.ready, pr.health.unhealthy = false, true
			}
		}
		unhealthy := pr.health.unhealthy
		p.mu.Unlock()
		if unhealthy {
			return // final until the reconciler restarts the container
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *prober) close() {
	p.cancel()
	p.wg.Wait()
}

// runCheck probes ip once. HTTP passes on any 2xx or 3xx response
// (redirects are not followed); TCP passes when a connection opens.
func runCheck(ctx context.Context, ip string, hc HealthCheck) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(hc.Port))
	if hc.HTTPPath == "" {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+hc.HTTPPath, nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain so the server isn't cut off mid-write
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
