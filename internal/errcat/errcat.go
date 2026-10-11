// Package errcat is the catalogue of error codes E01 to E15 (ADR 0017,
// section 4). Each entry has a stable code, a title, a message template with
// the concrete values, and a fix hint. Match maps the status reasons that
// the server emits to an entry, so that deploy, app status and the portal
// can show the same code and hint.
package errcat

import (
	"fmt"
	"slices"
	"strings"
)

// Entry is one catalogue error.
type Entry struct {
	Code     string
	Title    string
	Template string // human message; the %s verbs take the concrete values
	Hint     string
}

// Line is the one-line form used by deploy, status and the wait error:
// "E06: title — hint".
func (e Entry) Line() string {
	return e.Code + ": " + e.Title + " — " + e.Hint
}

// Message fills the template with the concrete values.
func (e Entry) Message(args ...any) string {
	return fmt.Sprintf(e.Template, args...)
}

var entries = []Entry{
	{"E01", "Image pull unauthorized", "pull %s: unauthorized",
		"Add a pull secret; image_pull_secret needs the new WorkloadSpec field (ADR 0017, open question 3)"},
	{"E02", "Image or tag not found", "pull %s: manifest unknown",
		"Check the tag; :latest is not pinned"},
	{"E03", "Edge port in use", "edge port %d is held by %s (pid %d); Shoal cannot serve HTTPS",
		"Stop the process, or move it. shoal init runs this check"},
	{"E04", "No edge node", "no node carries shoal.dev/edge; route %s will not be served",
		"shoal label node NAME shoal.dev/edge=true, or re-run init on the host"},
	{"E05", "DNS does not point at an edge", "%s resolves to %s; edges are %s",
		"Set the A record to an edge IP, and wait for the TTL"},
	{"E06", "ACME terms not accepted", "CertError: ACME terms of service not accepted (%s)",
		"Re-run init with --accept-tos (ADR 0017, section 1.5)"},
	{"E07", "HTTP-01 challenge fails, or rate limited", "certificate: HTTP-01 challenge for %s failed: %s; or rate limited until %s",
		"Open port 80 in the cloud firewall; after a rate limit, wait or use the staging directory"},
	{"E08", "Insufficient disk", "need %s for volume %s; %s free under /var/lib/shoal",
		"Free space, or reduce size"},
	{"E09", "Insufficient memory (Pending)", "replica %s is Pending: %s requested, %s has %s free",
		"Lower resources.memory, or add a node"},
	{"E10", "Volume's node is lost", "volume %s is on node %s, which is NotReady since %s",
		"Bring the node back, or restore a snapshot (ADR 0016, section 1.5)"},
	{"E11", "Database not ready or failed", "%s Failed: container exited %d: %s",
		"Check the volume owner and mode (ADR 0016, section 1.1); shoal app logs APP --db"},
	{"E12", "Missing Secret value", "secret %s key %s has no value",
		"shoal app secret set APP KEY"},
	{"E13", "Name or host owned by another object", "route %s is already used by app %s",
		"Pick another domain, or shoal app delete APP"},
	{"E14", "Crash loop", "replica %s crashed %d times; last exit %d: %q",
		"shoal app logs APP; fix the app or its env"},
	{"E15", "Health check failing", "replica %s not ready: GET %s on :%d returned %s",
		"Set health.path to a route the app serves"},
}

// All returns the catalogue in code order.
func All() []Entry {
	return slices.Clone(entries)
}

// Get returns the entry for a code such as "E07". The match ignores case and
// accepts a single digit ("e7").
func Get(code string) (*Entry, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) == 2 && code[0] == 'E' {
		code = "E0" + code[1:]
	}
	for i := range entries {
		if entries[i].Code == code {
			e := entries[i]
			return &e, true
		}
	}
	return nil, false
}

// Terminal reports whether a deploy wait should fail at once when the object
// is in e. A terminal state does not heal by itself: a person has to change
// the host's setup, the spec or the Secret first (ADR 0017, section 4.1).
// The rest are transient: the controller retries, or a node or replica may
// still come back, so the wait keeps going until its timeout.
func Terminal(e Entry) bool {
	switch e.Code {
	case "E06", "E11", "E12", "E13":
		return true
	}
	return false
}

// Match maps an object's status to a catalogue entry. kind is the object
// kind (Route, Database, Volume, or Replica for replica messages), state is
// its status state or phase, and message is its status message. The states
// are the ones the controller and the server set (route_status.go,
// controller/routes.go, controller/databases.go, controller/volumes.go); the
// messages are matched on the substrings that the code emits. ok is false
// when no entry fits, which is the normal case for a healthy object.
func Match(kind, state, message string) (*Entry, bool) {
	code := ""
	switch strings.ToLower(kind) {
	case "route":
		switch state {
		case "CertError":
			code = "E07"
			if isTermsError(message) {
				code = "E06"
			}
		case "NoEndpoints":
			code = "E15"
		case "Conflict":
			code = "E13"
		case "SecretInvalid", "SecretNotFound":
			code = "E12"
		}
	case "database":
		if state == "Failed" {
			code = "E11"
		}
	case "volume":
		if state == "NodeLost" {
			code = "E10"
		}
	}
	if code == "" {
		code = matchMessage(message)
	}
	if code == "" {
		return nil, false
	}
	return Get(code)
}

func isTermsError(message string) bool {
	return strings.Contains(message, "terms") && strings.Contains(message, "not accepted")
}

// matchMessage finds the code for a message from a replica, a pull or the
// scheduler, where the state carries no reason.
func matchMessage(message string) string {
	switch {
	case message == "":
		return ""
	case strings.Contains(message, "unauthorized"):
		return "E01"
	case strings.Contains(message, "manifest unknown"):
		return "E02"
	case strings.Contains(message, "unschedulable: no ready node has"):
		return "E09"
	case strings.Contains(message, "free under /var/lib"):
		return "E08"
	case strings.Contains(message, "crashed") && strings.Contains(message, "last exit"):
		return "E14"
	case strings.Contains(message, "healthz") || strings.Contains(message, "health check"):
		return "E15"
	}
	return ""
}
