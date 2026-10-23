package pipeline

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// When values (ADR 0022, section 2).
const (
	WhenOnSuccess = "on_success"
	WhenAlways    = "always"
	WhenManual    = "manual"
	WhenNever     = "never"
)

// Events a run can have, as the rules see them.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventManual      = "manual"
)

// Context is what the rules are evaluated against: the ref the run reads, and
// the paths its commit range changed.
type Context struct {
	Branch  string // the branch name; empty for a tag run
	Tag     bool   // the run reads a tag
	Event   string // push, pull_request or manual
	Changed []string
}

// Select evaluates a job's rules against a run. It returns the job's when and
// true when the job is created in the run, and false when no rule matches.
// Rules are read in order and the first match wins. A matching rule's own
// when applies, otherwise on_success. A job without rules keeps its own when.
func Select(job *v1alpha1.PipelineJob, ctx Context) (string, bool) {
	if len(job.GetRules()) == 0 {
		return whenOrDefault(job.GetWhen()), true
	}
	for _, r := range job.GetRules() {
		if c := r.GetCondition(); c != "" {
			cond, err := ParseCondition(c)
			if err != nil || !cond.Match(ctx) {
				continue
			}
		}
		if len(r.GetChanges()) > 0 && !MatchChanges(r.GetChanges(), ctx.Changed) {
			continue
		}
		return whenOrDefault(r.GetWhen()), true
	}
	return "", false
}

func whenOrDefault(w string) string {
	if w == "" {
		return WhenOnSuccess
	}
	return w
}

// Condition is a parsed if: expression, a list of clauses joined by &&.
type Condition []clause

type clause struct {
	key   string // branch, tag or event
	value string // the branch name, "true", or the event
}

// conditionGrammar describes the accepted forms, for error messages.
const conditionGrammar = `branch == "<name>", tag == true, event == "<push|pull_request|manual>", joined by &&`

var (
	clauseRe    = regexp.MustCompile(`^(branch|tag|event)\s*==\s*(.*?)\s*$`)
	branchNameR = regexp.MustCompile(`^"([A-Za-z0-9][A-Za-z0-9._/-]{0,254})"$`)
	eventR      = regexp.MustCompile(`^"(push|pull_request|manual)"$`)
)

// ParseCondition parses an if: expression. Anything outside the grammar is an
// error that names the clause.
func ParseCondition(s string) (Condition, error) {
	var out Condition
	for _, raw := range strings.Split(s, "&&") {
		text := strings.TrimSpace(raw)
		m := clauseRe.FindStringSubmatch(text)
		if m == nil {
			return nil, fmt.Errorf("if: %q is not supported (the grammar is %s)", text, conditionGrammar)
		}
		key, value := m[1], m[2]
		switch key {
		case "branch":
			b := branchNameR.FindStringSubmatch(value)
			if b == nil {
				return nil, fmt.Errorf("if: branch must be compared with a quoted branch name, got %s", value)
			}
			out = append(out, clause{key: key, value: b[1]})
		case "tag":
			if value != "true" {
				return nil, fmt.Errorf("if: tag must be compared with true, got %s", value)
			}
			out = append(out, clause{key: key, value: "true"})
		case "event":
			e := eventR.FindStringSubmatch(value)
			if e == nil {
				return nil, fmt.Errorf("if: event must be one of \"push\", \"pull_request\" or \"manual\", got %s", value)
			}
			out = append(out, clause{key: key, value: e[1]})
		}
	}
	return out, nil
}

// Match reports whether every clause holds for the run.
func (c Condition) Match(ctx Context) bool {
	for _, cl := range c {
		switch cl.key {
		case "branch":
			if ctx.Tag || ctx.Branch != cl.value {
				return false
			}
		case "tag":
			if !ctx.Tag {
				return false
			}
		case "event":
			if ctx.Event != cl.value {
				return false
			}
		}
	}
	return true
}

// ValidatePattern checks one changes glob. Patterns are relative to the
// repository and do not leave it.
func ValidatePattern(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("pattern is empty")
	case strings.HasPrefix(p, "/"), strings.Contains(p, "\\"):
		return fmt.Errorf("%q must be a relative path pattern", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not leave the repository", p)
		}
	}
	_, err := globRegexp(p)
	return err
}

// MatchChanges reports whether any changed path matches any pattern.
func MatchChanges(patterns, paths []string) bool {
	for _, p := range patterns {
		re, err := globRegexp(p)
		if err != nil {
			continue
		}
		for _, path := range paths {
			if re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// globRegexp turns a glob into a regular expression. * matches within one
// path segment, ** across segments, and ? one character that is not '/'.
// "**/" also matches no directory at all.
func globRegexp(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			if i+2 < len(p) && p[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
