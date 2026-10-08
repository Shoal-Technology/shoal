package containerd

import (
	"slices"
	"testing"
)

func TestSplitIptablesRule(t *testing.T) {
	line := `-A POSTROUTING -s 10.251.0.3/32 -m comment --comment "name: \"shoal-integration\" id: \"it-agent\"" -j CNI-1432f7eed4f17fc5d11b8530`
	got := splitIptablesRule(line)
	want := []string{"-A", "POSTROUTING", "-s", "10.251.0.3/32", "-m", "comment", "--comment",
		`name: "shoal-integration" id: "it-agent"`, "-j", "CNI-1432f7eed4f17fc5d11b8530"}
	if !slices.Equal(got, want) {
		t.Fatalf("split = %q\nwant    %q", got, want)
	}
	if !ruleHasComment(got, `name: "shoal-integration" id: "it-agent"`) {
		t.Error("comment not found")
	}
	// A longer ID with the same prefix must not match.
	if ruleHasComment(got, `name: "shoal-integration" id: "it-age"`) {
		t.Error("partial comment matched")
	}
	if got := splitIptablesRule(`-N CNI-x`); !slices.Equal(got, []string{"-N", "CNI-x"}) {
		t.Errorf("split = %q", got)
	}
	if got := splitIptablesRule(`-A X -m comment --comment ""`); len(got) != 6 || got[5] != "" {
		t.Errorf("empty quoted argument lost: %q", got)
	}
}
