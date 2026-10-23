package agent

import (
	"context"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// leaderPrefix starts the message of a FailedPrecondition that a server
// returns when it is not the leader; the leader's API address follows. It
// must match internal/server (cluster.go, leaderAddrMarker).
const leaderPrefix = "not the leader; leader API address is "

// retryPolicy bounds the reconnect backoff. wait is replaceable in tests.
type retryPolicy struct {
	min, max, denied time.Duration
	wait             func(context.Context, time.Duration) error
}

// policy returns the retry policy with the defaults filled in.
func (c *Connected) policy() retryPolicy {
	p := c.retry
	if p.min <= 0 {
		p.min = reconnectMin
	}
	if p.max <= 0 {
		p.max = reconnectMax
	}
	if p.denied <= 0 {
		p.denied = reconnectDenied
	}
	if p.wait == nil {
		p.wait = sleep
	}
	return p
}

// leaderHint returns the leader's address named by a follower's refusal.
func leaderHint(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return "", false
	}
	addr, ok := strings.CutPrefix(st.Message(), leaderPrefix)
	addr = strings.TrimSpace(addr)
	if !ok || addr == "" {
		return "", false
	}
	return addr, true
}

// retryable reports whether a failed join or config fetch is worth trying
// again, on another server or later. A follower with no known leader is
// FailedPrecondition too.
func retryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.FailedPrecondition:
		return true
	}
	return false
}

// normalizeServers trims the addresses and drops empty and repeated ones,
// keeping the order.
func normalizeServers(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// addServer appends addr unless it is already known.
func addServer(servers []string, addr string) []string {
	if addr == "" || slices.Contains(servers, addr) {
		return servers
	}
	return append(servers, addr)
}

// jittered returns a random wait in [d/2, d], so that agents which lost the
// same server don't retry in step.
func jittered(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + rand.N(d-half+1)
}

// pickServer returns the address the next session (or request) dials.
func (c *Connected) pickServer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

// moveOn is called when a connection to addr failed without a leader hint:
// the next attempt goes to the next known server, in order. Nothing moves
// if a hint already chose the next server.
func (c *Connected) moveOn(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.target != addr || len(c.servers) == 0 {
		return
	}
	i := slices.Index(c.servers, addr)
	c.target = c.servers[(i+1)%len(c.servers)]
}

// followHint makes the next attempt go to leader, the address a follower
// named. The leader is not trusted yet: the attempt dials it with the
// cluster CA, and it is only remembered once a session with it succeeds
// (see served). Hints that keep chaining without a success are refused, so
// that disagreeing followers cannot keep the agent busy; the caller backs
// off instead.
func (c *Connected) followHint(leader string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hops++
	if c.hops > len(c.servers)+1 {
		return false
	}
	c.target = leader
	return true
}

// served records that addr accepted a session or request, so it is the
// leader. It becomes the address dialed first, it is kept in the known
// servers, and the saved credentials name it, so a restart starts there.
func (c *Connected) served(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hops = 0
	c.target = addr
	c.servers = addServer(c.servers, addr)
	if c.creds == nil || c.creds.Server == addr {
		return
	}
	c.creds.Server = addr
	if c.cfg.CredentialsPath != "" {
		if err := c.creds.Save(c.cfg.CredentialsPath); err != nil {
			c.log.Warn("save leader address", "server", addr, "err", err)
		}
	}
}
