package resource

import (
	"net"
	"strings"
	"testing"
)

// FuzzRouteHost checks that hostProblem never panics, and that every host it
// accepts really is a lowercase DNS name of at least two labels, within the
// length limit, and not an IP address.
func FuzzRouteHost(f *testing.F) {
	seeds := []string{
		"app.example.com",
		"a.b",
		"xn--bcher-kva.example",
		"example.com.",
		"",
		"*.example.com",
		"APP.example.com",
		"localhost",
		"10.0.0.1",
		"::1",
		"1.2.3",
		"example.123",
		"-bad.example.com",
		"bad-.example.com",
		"under_score.example.com",
		"a..b",
		strings.Repeat("a", 64) + ".example.com",
		strings.Repeat("a.", 130) + "com",
		"app.example.com\x00",
		"app.example.com:443",
		"exämple.com",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		if hostProblem(h) != "" {
			return
		}
		if len(h) > maxRouteHostLen || h != strings.ToLower(h) || strings.HasSuffix(h, ".") {
			t.Fatalf("accepted host %q breaks length, case or trailing-dot rules", h)
		}
		if net.ParseIP(h) != nil {
			t.Fatalf("accepted IP address %q", h)
		}
		labels := strings.Split(h, ".")
		if len(labels) < 2 {
			t.Fatalf("accepted single-label host %q", h)
		}
		for _, l := range labels {
			if !hostLabel.MatchString(l) {
				t.Fatalf("accepted host %q with bad label %q", h, l)
			}
		}
	})
}
