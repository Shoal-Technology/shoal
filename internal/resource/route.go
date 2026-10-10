package resource

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
)

// Route limits (ADR 0015).
const (
	maxRouteHostLen = 253
	maxRoutePathLen = 256
)

// hostLabel is one lowercase DNS label.
var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// allDigits matches a top-level label that is all digits. Such a name is not
// a valid host, and it also catches IPv4 addresses that net.ParseIP misses
// when they are written oddly.
var allDigits = regexp.MustCompile(`^[0-9]+$`)

// Route tls modes.
const (
	tlsAuto   = "auto"
	tlsNone   = "none"
	tlsSecret = "secret"
)

// DefaultRoute fills in tls "auto", pathPrefix "/", and redirectHttp, which
// is true unless tls is none.
func DefaultRoute(s *v1alpha1.RouteSpec) {
	if s.Tls == "" {
		s.Tls = tlsAuto
	}
	if s.PathPrefix == "" {
		s.PathPrefix = "/"
	}
	if s.RedirectHttp == nil {
		s.RedirectHttp = proto.Bool(s.Tls != tlsNone)
	}
}

// RouteKey is the host and path prefix a Route claims. Two Routes with the
// same key conflict. It is only meaningful after DefaultRoute.
func RouteKey(s *v1alpha1.RouteSpec) string {
	return s.GetHost() + " " + s.GetPathPrefix()
}

// hostProblem returns why h is not an acceptable Route host, or "".
func hostProblem(h string) string {
	switch {
	case h == "":
		return "required"
	case strings.Contains(h, "*"):
		return "wildcard hosts are not supported in v1; list each host separately"
	case h != strings.ToLower(h):
		return "must be lowercase"
	case len(h) > maxRouteHostLen:
		return fmt.Sprintf("at most %d characters", maxRouteHostLen)
	case strings.HasSuffix(h, "."):
		return "must not end with a dot"
	case net.ParseIP(h) != nil:
		return "must be a DNS name, not an IP address"
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "must have at least two labels, for example app.example.com"
	}
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return fmt.Sprintf("invalid label %q: letters, digits and '-' only, not starting or ending with '-'", l)
		}
	}
	if allDigits.MatchString(labels[len(labels)-1]) {
		return "the last label must not be all digits"
	}
	return ""
}

// ValidateRoute checks a defaulted Route spec. Service and Secret references
// are only checked by name here: like Workload and Service references, they
// are resolved later, not at apply time.
func ValidateRoute(name string, s *v1alpha1.RouteSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, 63) {
		bad("metadata.name", "must be a lowercase DNS label of at most 63 characters")
	}
	if msg := hostProblem(s.GetHost()); msg != "" {
		bad("spec.host", "%s", msg)
	}
	switch p := s.GetPathPrefix(); {
	case !strings.HasPrefix(p, "/"):
		bad("spec.pathPrefix", "must start with /")
	case len(p) > maxRoutePathLen:
		bad("spec.pathPrefix", "at most %d characters", maxRoutePathLen)
	case strings.ContainsAny(p, "?#"):
		bad("spec.pathPrefix", "must not contain ? or #")
	}
	if !ValidName(s.GetService(), 63) {
		bad("spec.service", "must be a Service name (lowercase DNS label of at most 63 characters)")
	}
	if !validPort(s.GetPort()) {
		bad("spec.port", "must be between 1 and 65535")
	}
	switch s.GetTls() {
	case tlsAuto, tlsNone:
		if s.GetTlsSecret() != "" {
			bad("spec.tlsSecret", "only allowed with tls: secret")
		}
	case tlsSecret:
		if !ValidName(s.GetTlsSecret(), 63) {
			bad("spec.tlsSecret", "required with tls: secret; the name of a Secret holding the PEM certificate and key")
		}
	default:
		bad("spec.tls", "must be auto, none or secret")
	}
	if s.GetTls() == tlsNone && s.GetRedirectHttp() {
		bad("spec.redirectHttp", "not allowed with tls: none")
	}
	return errors.Join(errs...)
}
