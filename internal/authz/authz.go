// Package authz decides whether a caller may run an API verb on a kind in a
// project (ADR 0023). It holds the built-in roles and the decision. It does
// not read the store: the caller passes in the bindings. In pass A there are
// no stored bindings, so only the admin certificate can act, and the server
// runs in audit mode, which logs would-deny decisions instead of refusing.
package authz

import (
	"fmt"
	"slices"

	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// Verbs of the API. Apply is a create or an update of each object it names.
const (
	VerbGet    = "get"
	VerbList   = "list"
	VerbCreate = "create"
	VerbUpdate = "update"
	VerbDelete = "delete"
	// VerbAdmin is for cluster operations: backups, join tokens, node labels
	// and the portal password.
	VerbAdmin = "admin"
)

// Wildcard matches every verb or every kind in a role rule.
const Wildcard = "*"

// AdminCertRole is the certificate role of the admin key (ADR 0007). The
// admin key is cluster-admin: it is the break-glass path (ADR 0023, section
// 6).
const AdminCertRole = "shoal:admin"

// Built-in role names.
const (
	RoleClusterAdmin = "cluster-admin"
	RoleAdmin        = "admin"
	RoleEdit         = "edit"
	RoleView         = "view"
)

// rule grants verbs on kinds.
type rule struct {
	verbs []string
	kinds []string
}

// roles are the built-in roles. They are fixed in code: there is no stored
// Role kind yet.
var roles = map[string][]rule{
	RoleClusterAdmin: {{verbs: []string{Wildcard}, kinds: []string{Wildcard}}},
	RoleAdmin: {{
		verbs: []string{VerbGet, VerbList, VerbCreate, VerbUpdate, VerbDelete},
		kinds: tenancy.ScopedKinds(),
	}},
	RoleEdit: {{
		verbs: []string{VerbGet, VerbList, VerbCreate, VerbUpdate, VerbDelete},
		kinds: withoutKinds(tenancy.ScopedKinds(), "ResourceQuota"),
	}},
	RoleView: {{
		verbs: []string{VerbGet, VerbList},
		kinds: tenancy.ScopedKinds(),
	}},
}

// withoutKinds returns kinds minus the excluded ones.
func withoutKinds(kinds []string, exclude ...string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !slices.Contains(exclude, k) {
			out = append(out, k)
		}
	}
	return out
}

// Roles returns the names of the built-in roles, sorted.
func Roles() []string {
	out := make([]string, 0, len(roles))
	for r := range roles {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// Subject is the caller of an API call.
type Subject struct {
	// User is the certificate's name (a user, once user certificates exist).
	// Empty for the admin key.
	User string
	// Groups are the groups of the user.
	Groups []string
	// CertRole is the certificate role (security.RoleAdmin or security.RoleNode).
	CertRole string
}

// Request is one API call to decide.
type Request struct {
	Subject Subject
	Verb    string
	// Kind is empty for a call with no kind (cluster operations).
	Kind string
	// Project is empty for cluster-wide calls and for calls across all
	// projects.
	Project string
}

// Binding gives a role to a user or a group, in one project. An empty Project
// is cluster-wide. Exactly one of User and Group is set.
type Binding struct {
	User    string
	Group   string
	Role    string
	Project string
}

// Decision is the outcome of Decide. Reason is for the log and the error.
type Decision struct {
	Allowed bool
	Reason  string
}

// Decide reports whether req is allowed. The admin certificate is allowed
// everything. Otherwise a binding must give a role that grants the verb on
// the kind, in the request's project (a project-scoped binding never applies
// to a cluster-wide or all-projects call). Anything else is denied.
func Decide(req Request, bindings []Binding) Decision {
	if req.Subject.CertRole == AdminCertRole {
		return Decision{Allowed: true, Reason: "admin certificate (cluster-admin)"}
	}
	if req.Subject.User == "" && len(req.Subject.Groups) == 0 {
		return Decision{Reason: "no identity"}
	}
	for _, b := range bindings {
		if !appliesTo(b, req.Subject) {
			continue
		}
		if b.Project != "" && b.Project != req.Project {
			continue
		}
		if roleAllows(b.Role, req.Verb, req.Kind) {
			return Decision{Allowed: true, Reason: fmt.Sprintf("role %s in project %s", b.Role, scope(b.Project))}
		}
	}
	return Decision{Reason: fmt.Sprintf("no binding grants %s on %s in project %s", req.Verb, kindOr(req.Kind), scope(req.Project))}
}

func appliesTo(b Binding, s Subject) bool {
	if b.User != "" {
		return b.User == s.User
	}
	return b.Group != "" && slices.Contains(s.Groups, b.Group)
}

// roleAllows reports whether role has a rule for verb on kind. An unknown
// role grants nothing. Kind is "" for a cluster-level call; only a rule with
// the wildcard kind matches it.
func roleAllows(role, verb, kind string) bool {
	for _, r := range roles[role] {
		if !slices.Contains(r.verbs, Wildcard) && !slices.Contains(r.verbs, verb) {
			continue
		}
		if slices.Contains(r.kinds, Wildcard) || (kind != "" && slices.Contains(r.kinds, kind)) {
			return true
		}
	}
	return false
}

func scope(project string) string {
	if project == "" {
		return "(all or cluster-wide)"
	}
	return project
}

func kindOr(kind string) string {
	if kind == "" {
		return "the cluster"
	}
	return kind
}

// Mode is how the server acts on a decision.
type Mode string

const (
	// ModeAudit logs a would-deny decision and lets the call go on. It is the
	// default in pass A (ADR 0023, section 6).
	ModeAudit Mode = "audit"
	// ModeEnforce refuses a denied call.
	ModeEnforce Mode = "enforce"
)

// ParseMode accepts "" (the default, audit), "audit" and "enforce".
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeAudit:
		return ModeAudit, nil
	case ModeEnforce:
		return ModeEnforce, nil
	}
	return "", fmt.Errorf("authorization mode %q: want audit or enforce", s)
}

// Blocks reports whether the mode refuses a call with decision d.
func (m Mode) Blocks(d Decision) bool {
	return m == ModeEnforce && !d.Allowed
}

// IsMutating reports whether verb changes state, so that the call is written
// to the audit log whether or not it is allowed.
func IsMutating(verb string) bool {
	switch verb {
	case VerbCreate, VerbUpdate, VerbDelete, VerbAdmin:
		return true
	}
	return false
}
