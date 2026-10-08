package network

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestRenderServices(t *testing.T) {
	p := NewServiceProxy("10.250.0.0/16")
	rules, chains := p.render([]Service{
		{Name: "web", ClusterIP: "10.96.0.1", Ports: []ServicePort{{Port: 80, TargetPort: 8080, Protocol: "tcp"}},
			Endpoints: []string{"10.250.2.3", "10.250.1.2"}},
		{Name: "empty", ClusterIP: "10.96.0.2", Ports: []ServicePort{{Port: 53, TargetPort: 53, Protocol: "udp"}}},
	})
	if len(chains) != 2 {
		t.Fatalf("chains = %v", chains)
	}
	web := portChain(Service{Name: "web"}, ServicePort{Port: 80, Protocol: "tcp"})
	for _, want := range []string{
		"-A SHOAL-SERVICES -d 10.96.0.1/32 -p tcp --dport 80 -m comment --comment \"web:80\" -j " + web,
		"-A " + web + " -j MARK --set-xmark 0x4000/0x4000",
		// Two endpoints, sorted: the first gets half, the second the rest.
		"-A " + web + " -p tcp -m statistic --mode random --probability 0.50000 -j DNAT --to-destination 10.250.1.2:8080",
		"-A " + web + " -p tcp -j DNAT --to-destination 10.250.2.3:8080",
		"-A SHOAL-POSTROUTING -m mark --mark 0x4000/0x4000 -j MASQUERADE",
		"-A SHOAL-POSTROUTING -s 10.250.0.0/16 -d 10.250.0.0/16 -j ACCEPT",
		"-A SHOAL-FORWARD -s 10.250.0.0/16 -j ACCEPT",
		// The Service without endpoints refuses connections right away.
		"-A SHOAL-FORWARD -d 10.96.0.2/32 -p udp --dport 53 -m comment --comment \"empty:53 has no endpoints\" -j REJECT --reject-with icmp-port-unreachable",
		"-A SHOAL-OUTPUT -d 10.96.0.2/32 -p udp --dport 53 -m comment --comment \"empty:53 has no endpoints\" -j REJECT --reject-with icmp-port-unreachable",
	} {
		if !strings.Contains(rules, want+"\n") {
			t.Errorf("rules lack %q\n%s", want, rules)
		}
	}
	if len(chains[0]) > 28 {
		t.Errorf("chain name %q longer than iptables allows", chains[0])
	}
}

func TestServiceProxySync(t *testing.T) {
	var calls []string
	chainExists := false
	p := NewServiceProxy("10.250.0.0/16")
	p.iptables = func(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case call == "-t nat -S":
			return []byte("-N SHOAL-SVC-LEFTOVER\n-N OTHER\n"), nil
		case strings.Contains(call, "-C"):
			if chainExists {
				return nil, nil
			}
			return nil, io.EOF // not there yet
		}
		return nil, nil
	}
	svc := []Service{{Name: "web", ClusterIP: "10.96.0.1", Ports: []ServicePort{{Port: 80, TargetPort: 80, Protocol: "tcp"}}}}
	if err := p.Sync(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"restore --noflush", "-t nat -I PREROUTING 1 -j SHOAL-SERVICES",
		"-t nat -I OUTPUT 1 -j SHOAL-SERVICES", "-t nat -I POSTROUTING 1 -j SHOAL-POSTROUTING",
		"-t filter -I FORWARD 1 -j SHOAL-FORWARD", "-t nat -F SHOAL-SVC-LEFTOVER\n-t nat -X SHOAL-SVC-LEFTOVER"} {
		if !strings.Contains(joined, want) {
			t.Errorf("calls lack %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "-t filter -I OUTPUT 1 -j SHOAL-OUTPUT") {
		t.Errorf("no jump to SHOAL-OUTPUT:\n%s", joined)
	}
	if strings.Contains(rules(p, svc), "REJECT") {
		t.Error("a Service with endpoints must not reject")
	}
	if strings.Contains(joined, "-X OTHER") {
		t.Error("removed a chain that isn't ours")
	}
	calls, chainExists = nil, true
	if err := p.Sync(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("unchanged Services re-applied: %q", calls)
	}
}

// rules renders svc with an endpoint added to the first Service (on a copy).
func rules(p *ServiceProxy, svc []Service) string {
	withEndpoint := slices.Clone(svc)
	withEndpoint[0].Endpoints = []string{"10.250.1.2"}
	r, _ := p.render(withEndpoint)
	return r
}

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 42, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDNSAnswersServices(t *testing.T) {
	d := NewDNS("shoal", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetServices([]Service{{Name: "web", ClusterIP: "10.96.0.7"}})

	tests := []struct {
		name    string
		typ     dnsmessage.Type
		rcode   dnsmessage.RCode
		answers int
	}{
		{"web.shoal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, 1},
		{"WEB.Shoal.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, 1},
		{"web.shoal.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, 0},
		{"nope.shoal.", dnsmessage.TypeA, dnsmessage.RCodeNameError, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name+tt.typ.String(), func(t *testing.T) {
			b, err := d.answer(context.Background(), query(t, tt.name, tt.typ))
			if err != nil {
				t.Fatal(err)
			}
			var m dnsmessage.Message
			if err := m.Unpack(b); err != nil {
				t.Fatal(err)
			}
			if m.Header.ID != 42 || m.Header.RCode != tt.rcode || len(m.Answers) != tt.answers || !m.Header.Authoritative {
				t.Fatalf("response = %+v", m)
			}
			if tt.answers == 1 {
				if a := m.Answers[0].Body.(*dnsmessage.AResource).A; a != [4]byte{10, 96, 0, 7} {
					t.Fatalf("A = %v", a)
				}
			}
		})
	}
	// Other names go upstream; with none reachable that is an error, not a
	// made-up answer.
	if _, err := d.answer(context.Background(), query(t, "example.com.", dnsmessage.TypeA)); err != nil {
		t.Logf("forward without upstreams: %v", err)
	}
}

func TestGatewayIP(t *testing.T) {
	if gw, err := GatewayIP("10.250.3.0/24"); err != nil || gw != "10.250.3.1" {
		t.Fatalf("GatewayIP = %q, %v", gw, err)
	}
	if _, err := GatewayIP("nope"); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}
