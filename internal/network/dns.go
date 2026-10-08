package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNS answers <service>.<domain> with the Service's IP and forwards every
// other query to the host's resolvers. Containers on the node use it as
// their only nameserver.
type DNS struct {
	domain   string // e.g. "shoal."
	upstream []string
	log      *slog.Logger

	mu      sync.RWMutex
	records map[string]netip.Addr // fully qualified, lowercase
}

// recordTTL is short: Service IPs are stable, but Services come and go.
const recordTTL = 5

// NewDNS returns a resolver for domain that forwards to upstream (host:port
// addresses).
func NewDNS(domain string, upstream []string, log *slog.Logger) *DNS {
	return &DNS{domain: strings.ToLower(strings.TrimSuffix(domain, ".")) + ".", upstream: upstream, log: log, records: map[string]netip.Addr{}}
}

// HostResolvers returns the nameservers of the host's /etc/resolv.conf as
// host:port. The agent runs in the host network namespace, so
// systemd-resolved's 127.0.0.53 is reachable from it.
func HostResolvers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return []string{"127.0.0.53:53"}
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			out = append(out, net.JoinHostPort(f[1], "53"))
		}
	}
	if len(out) == 0 {
		out = []string{"127.0.0.53:53"}
	}
	return out
}

// SetServices replaces the records: Service name -> IP.
func (d *DNS) SetServices(services []Service) {
	records := make(map[string]netip.Addr, len(services))
	for _, s := range services {
		if ip, err := netip.ParseAddr(s.ClusterIP); err == nil {
			records[strings.ToLower(s.Name)+"."+d.domain] = ip
		}
	}
	d.mu.Lock()
	d.records = records
	d.mu.Unlock()
}

// ListenAndServe answers queries on addr (UDP) until ctx is done. The
// address may not exist yet (the bridge appears with the first container),
// so the socket uses IP_FREEBIND.
func (d *DNS) ListenAndServe(ctx context.Context, addr string) error {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, 15 /* IP_FREEBIND */, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", addr)
	if err != nil {
		return fmt.Errorf("DNS: listen on %s: %w", addr, err)
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("DNS: read: %w", err)
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			resp, err := d.answer(ctx, query)
			if err != nil {
				d.log.Debug("DNS query failed", "err", err)
				return
			}
			_, _ = pc.WriteTo(resp, from) // the client retries on loss
		}()
	}
}

// answer resolves one query packet.
func (d *DNS) answer(ctx context.Context, query []byte) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(q.Name.String())
	if name != d.domain && !strings.HasSuffix(name, "."+d.domain) {
		return d.forward(ctx, query)
	}
	// Authoritative for the Service domain.
	resp := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true},
		Questions: []dnsmessage.Question{q},
	}
	d.mu.RLock()
	ip, ok := d.records[name]
	d.mu.RUnlock()
	switch {
	case !ok:
		resp.Header.RCode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL:
		resp.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: recordTTL},
			Body:   &dnsmessage.AResource{A: ip.As4()},
		}}
	}
	// Other types for a known name: an empty NOERROR answer.
	return resp.Pack()
}

// forward relays a query to the upstream resolvers in turn.
func (d *DNS) forward(ctx context.Context, query []byte) ([]byte, error) {
	var errs []error
	for _, up := range d.upstream {
		resp, err := exchange(ctx, up, query)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

func exchange(ctx context.Context, server string, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
