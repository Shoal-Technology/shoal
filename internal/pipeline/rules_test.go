package pipeline

import (
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// rule builds a rule for a test job.
func rule(cond string, changes []string, when string) *v1alpha1.PipelineRule {
	return &v1alpha1.PipelineRule{Condition: cond, Changes: changes, When: when}
}

func TestSelectFirstMatchWins(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "j", Rules: []*v1alpha1.PipelineRule{
		rule(`branch == "main"`, nil, WhenManual),
		rule(`branch == "main"`, nil, WhenAlways),
	}}
	when, ok := Select(job, Context{Branch: "main", Event: EventPush})
	if !ok || when != WhenManual {
		t.Fatalf("first matching rule must set when: got %q, %v", when, ok)
	}
}

func TestSelectNoMatchOmitsTheJob(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "j", Rules: []*v1alpha1.PipelineRule{
		rule(`branch == "main"`, nil, ""),
	}}
	if _, ok := Select(job, Context{Branch: "dev", Event: EventPush}); ok {
		t.Fatal("no rule matched: the job must not be created in the run")
	}
}

func TestSelectRuleWithoutWhenIsOnSuccess(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "j", When: WhenAlways, Rules: []*v1alpha1.PipelineRule{
		rule(`branch == "main"`, nil, ""),
	}}
	when, ok := Select(job, Context{Branch: "main", Event: EventPush})
	if !ok || when != WhenOnSuccess {
		t.Fatalf("a matching rule without when is on_success, not the job's when: got %q", when)
	}
}

func TestSelectJobWithoutRulesKeepsItsWhen(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "j", When: WhenManual}
	when, ok := Select(job, Context{Branch: "anything", Event: EventManual})
	if !ok || when != WhenManual {
		t.Fatalf("no rules: the job runs with its own when: got %q, %v", when, ok)
	}
	plain := &v1alpha1.PipelineJob{Name: "p"}
	if when, _ := Select(plain, Context{}); when != WhenOnSuccess {
		t.Fatalf("no rules and no when: on_success, got %q", when)
	}
}

func TestSelectWhenManualAndNever(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "deploy", Rules: []*v1alpha1.PipelineRule{
		rule(`branch == "main"`, nil, WhenManual),
	}}
	if when, ok := Select(job, Context{Branch: "main", Event: EventPush}); !ok || when != WhenManual {
		t.Fatalf("manual: got %q, %v", when, ok)
	}
	never := &v1alpha1.PipelineJob{Name: "n", Rules: []*v1alpha1.PipelineRule{rule(`event == "push"`, nil, WhenNever)}}
	if when, ok := Select(never, Context{Event: EventPush}); !ok || when != WhenNever {
		t.Fatalf("never: got %q, %v", when, ok)
	}
}

func TestSelectEveryIfForm(t *testing.T) {
	cases := []struct {
		name string
		cond string
		ctx  Context
		want bool
	}{
		{"branch matches", `branch == "main"`, Context{Branch: "main", Event: EventPush}, true},
		{"branch differs", `branch == "main"`, Context{Branch: "dev", Event: EventPush}, false},
		{"branch never matches a tag run", `branch == "main"`, Context{Tag: true, Branch: "main", Event: EventPush}, false},
		{"tag matches a tag run", `tag == true`, Context{Tag: true, Event: EventPush}, true},
		{"tag false on a branch run", `tag == true`, Context{Branch: "main", Event: EventPush}, false},
		{"event push", `event == "push"`, Context{Event: EventPush}, true},
		{"event pull_request", `event == "pull_request"`, Context{Event: EventPullRequest}, true},
		{"event manual differs from push", `event == "manual"`, Context{Event: EventPush}, false},
		{"and of two clauses", `branch == "main" && event == "push"`, Context{Branch: "main", Event: EventPush}, true},
		{"and fails when one clause fails", `branch == "main" && event == "pull_request"`, Context{Branch: "main", Event: EventPush}, false},
		{"and with tag", `tag == true && event == "push"`, Context{Tag: true, Event: EventPush}, true},
		{"spaces are free", `  branch   ==   "release/1.2"  `, Context{Branch: "release/1.2", Event: EventPush}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &v1alpha1.PipelineJob{Name: "j", Rules: []*v1alpha1.PipelineRule{rule(tc.cond, nil, "")}}
			_, ok := Select(job, tc.ctx)
			if ok != tc.want {
				t.Fatalf("Select(%q, %+v) = %v, want %v", tc.cond, tc.ctx, ok, tc.want)
			}
		})
	}
}

func TestParseConditionRefusesOutsideTheGrammar(t *testing.T) {
	for _, bad := range []string{
		`branch != "main"`,
		`branch =~ "ma.*"`,
		`branch == main`,
		`tag == false`,
		`event == "deploy"`,
		`pipeline == "x"`,
		`variables.X == "1"`,
		`branch == "main" || tag == true`,
		`branch == "main" && `,
	} {
		if _, err := ParseCondition(bad); err == nil {
			t.Errorf("ParseCondition(%q) must be refused", bad)
		}
	}
	if _, err := ParseCondition(`branch == "main" && tag == true`); err != nil {
		t.Errorf("a valid condition was refused: %v", err)
	}
}

func TestChangesGlobs(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"web/**", "web/a.go", true},
		{"web/**", "web/src/deep/a.go", true},
		{"web/**", "api/a.go", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/guide/a.md", true},
		{"**/*.md", "docs/a.txt", false},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "go.sum", false},
		{"web/**/*.ts", "web/x/y/z.ts", true},
	}
	for _, tc := range cases {
		if got := MatchChanges([]string{tc.pattern}, []string{tc.path}); got != tc.want {
			t.Errorf("MatchChanges(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestChangesRuleNeedsAMatchingPath(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "web", Rules: []*v1alpha1.PipelineRule{rule("", []string{"web/**"}, "")}}
	if _, ok := Select(job, Context{Event: EventPush, Changed: []string{"api/x.go"}}); ok {
		t.Error("no changed path matches web/**: the job must be omitted")
	}
	if _, ok := Select(job, Context{Event: EventPush, Changed: []string{"api/x.go", "web/app.ts"}}); !ok {
		t.Error("web/app.ts matches web/**: the job must be created")
	}
}

func TestChangesAndIfTogether(t *testing.T) {
	job := &v1alpha1.PipelineJob{Name: "j", Rules: []*v1alpha1.PipelineRule{rule(`branch == "main"`, []string{"web/**"}, "")}}
	if _, ok := Select(job, Context{Branch: "main", Event: EventPush, Changed: []string{"web/a"}}); !ok {
		t.Error("both hold: the job must be created")
	}
	if _, ok := Select(job, Context{Branch: "dev", Event: EventPush, Changed: []string{"web/a"}}); ok {
		t.Error("the if fails: the job must be omitted")
	}
	if _, ok := Select(job, Context{Branch: "main", Event: EventPush, Changed: []string{"api/a"}}); ok {
		t.Error("the changes fail: the job must be omitted")
	}
}

func TestValidatePattern(t *testing.T) {
	for _, bad := range []string{"", "/abs/**", "../up/**", `web\x`} {
		if err := ValidatePattern(bad); err == nil {
			t.Errorf("ValidatePattern(%q) must be refused", bad)
		}
	}
	if err := ValidatePattern("web/**"); err != nil {
		t.Errorf("web/** is valid: %v", err)
	}
}
