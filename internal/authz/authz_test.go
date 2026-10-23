package authz

import "testing"

var (
	adminKey = Subject{CertRole: AdminCertRole}
	nodeCert = Subject{CertRole: "shoal:node"}
	alice    = Subject{User: "alice", Groups: []string{"dev"}, CertRole: "shoal:user"}
)

func TestDecideTable(t *testing.T) {
	bindings := []Binding{
		{User: "alice", Role: RoleEdit, Project: "team-a"},
		{Group: "ops", Role: RoleView, Project: ""},
		{User: "bob", Role: RoleAdmin, Project: "team-b"},
	}
	ops := Subject{User: "carol", Groups: []string{"ops"}, CertRole: "shoal:user"}
	bob := Subject{User: "bob", CertRole: "shoal:user"}

	cases := []struct {
		name     string
		subject  Subject
		req      Request
		bindings []Binding
		allowed  bool
	}{
		{"admin key may do anything", adminKey, Request{Verb: VerbDelete, Kind: "Workload", Project: "team-a"}, nil, true},
		{"admin key may run a cluster operation", adminKey, Request{Verb: VerbAdmin}, nil, true},
		{"node certificate has no binding", nodeCert, Request{Verb: VerbGet, Kind: "Workload", Project: "default"}, nil, false},
		{"no identity is denied", Subject{}, Request{Verb: VerbGet, Kind: "Workload"}, bindings, false},
		{"edit may create in its project", alice, Request{Verb: VerbCreate, Kind: "Workload", Project: "team-a"}, bindings, true},
		{"edit may not write another project", alice, Request{Verb: VerbCreate, Kind: "Workload", Project: "team-b"}, bindings, false},
		{"edit may not list across projects", alice, Request{Verb: VerbList, Kind: "Workload", Project: ""}, bindings, false},
		{"edit may not write quotas", alice, Request{Verb: VerbUpdate, Kind: "ResourceQuota", Project: "team-a"}, bindings, false},
		{"edit may not run cluster operations", alice, Request{Verb: VerbAdmin, Project: "team-a"}, bindings, false},
		{"edit may not write Project objects", alice, Request{Verb: VerbCreate, Kind: "Project", Project: "team-a"}, bindings, false},
		{"view may read in any project through a cluster-wide group binding", ops, Request{Verb: VerbGet, Kind: "Secret", Project: "team-b"}, bindings, true},
		{"view may list all projects", ops, Request{Verb: VerbList, Kind: "Workload", Project: ""}, bindings, true},
		{"view may not write", ops, Request{Verb: VerbUpdate, Kind: "Workload", Project: "team-a"}, bindings, false},
		{"admin role may write its project", bob, Request{Verb: VerbUpdate, Kind: "Volume", Project: "team-b"}, bindings, true},
		{"admin role may not delete another project", bob, Request{Verb: VerbDelete, Kind: "Volume", Project: "team-a"}, bindings, false},
		{"no bindings denies everyone", alice, Request{Verb: VerbGet, Kind: "Workload", Project: "team-a"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			req.Subject = c.subject
			got := Decide(req, c.bindings)
			if got.Allowed != c.allowed {
				t.Fatalf("allowed = %v (%s), want %v", got.Allowed, got.Reason, c.allowed)
			}
			if got.Reason == "" {
				t.Fatal("decision has no reason")
			}
		})
	}
}

func TestUnknownRoleGrantsNothing(t *testing.T) {
	d := Decide(Request{Subject: alice, Verb: VerbGet, Kind: "Workload", Project: "team-a"},
		[]Binding{{User: "alice", Role: "root", Project: "team-a"}})
	if d.Allowed {
		t.Fatalf("unknown role allowed: %s", d.Reason)
	}
}

func TestParseModeAndBlocks(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAudit, "audit": ModeAudit, "enforce": ModeEnforce} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("log"); err == nil {
		t.Fatal("ParseMode accepted an unknown mode")
	}
	deny := Decision{Allowed: false, Reason: "no binding"}
	if ModeAudit.Blocks(deny) {
		t.Fatal("audit mode blocked a call")
	}
	if !ModeEnforce.Blocks(deny) {
		t.Fatal("enforce mode let a denied call through")
	}
	if ModeEnforce.Blocks(Decision{Allowed: true}) {
		t.Fatal("enforce mode blocked an allowed call")
	}
}

func TestIsMutating(t *testing.T) {
	for _, v := range []string{VerbCreate, VerbUpdate, VerbDelete, VerbAdmin} {
		if !IsMutating(v) {
			t.Errorf("%s should be mutating", v)
		}
	}
	for _, v := range []string{VerbGet, VerbList} {
		if IsMutating(v) {
			t.Errorf("%s should not be mutating", v)
		}
	}
}
