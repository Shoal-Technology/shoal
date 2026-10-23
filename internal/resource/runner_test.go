package resource

import (
	"slices"
	"testing"
)

func TestNodeMatchesTags(t *testing.T) {
	labels := map[string]string{RunnerLabel: "true", "linux-amd64": "true", "gpu": "false"}
	cases := []struct {
		tags []string
		want bool
	}{
		{nil, true},
		{[]string{"linux-amd64"}, true},
		{[]string{"linux-amd64", "gpu"}, false}, // gpu is labelled false
		{[]string{"arm64"}, false},
	}
	for _, c := range cases {
		if got := NodeMatchesTags(labels, c.tags); got != c.want {
			t.Errorf("NodeMatchesTags(%v) = %v, want %v", c.tags, got, c.want)
		}
	}
}

func TestIsRunnerNeedsTheTrueValue(t *testing.T) {
	if !IsRunner(map[string]string{RunnerLabel: "true"}) {
		t.Error("a node labelled runner=true is not a runner")
	}
	if IsRunner(map[string]string{RunnerLabel: "false"}) || IsRunner(nil) {
		t.Error("a node without runner=true is a runner")
	}
}

func TestTrustOfPush(t *testing.T) {
	if got := TrustOfPush("main", DefaultProtectedBranches); got != TrustProtected {
		t.Errorf("push to main = %s, want protected", got)
	}
	if got := TrustOfPush("feature/x", DefaultProtectedBranches); got != TrustSameRepo {
		t.Errorf("push to a feature branch = %s, want same-repo", got)
	}
}

func TestParsePushCollectsChangedPaths(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567",
		"commits":[{"added":["web/new.js"],"modified":["web/app.js"],"removed":[]},
		           {"added":[],"modified":["web/app.js","README.md"],"removed":["old.txt"]}]}`)
	ev, err := parsePush(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"web/new.js", "web/app.js", "README.md", "old.txt"}
	if !slices.Equal(ev.ChangedPaths, want) {
		t.Errorf("changed paths = %v, want %v", ev.ChangedPaths, want)
	}
}
