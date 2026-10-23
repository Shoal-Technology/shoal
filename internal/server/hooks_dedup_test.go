package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/internal/resource"
)

const hookTestOtherCommit = "fedcba9876543210fedcba9876543210fedcba98"

// TestWebhookRedeliveryIsDuplicate: a redelivered push of a commit that already
// has a Build starts nothing and names the existing Build.
func TestWebhookRedeliveryIsDuplicate(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	body := pushBody("refs/heads/main")
	status, out := f.post(githubHeaders("push", hookSign(body)), body)
	if status != http.StatusAccepted || !strings.Contains(out, `"started"`) {
		t.Fatalf("first push = %d %s, want 202 started", status, out)
	}
	first := f.buildNameFrom(t, out)

	status, out = f.post(githubHeaders("push", hookSign(body)), body)
	if status != http.StatusAccepted || !strings.Contains(out, `"duplicate"`) {
		t.Fatalf("redelivery = %d %s, want 202 duplicate", status, out)
	}
	if got := f.buildNameFrom(t, out); got != first {
		t.Fatalf("duplicate names build %q, want the existing %q", got, first)
	}
	if n := len(f.builds()); n != 1 {
		t.Fatalf("%d builds after a redelivery, want 1", n)
	}
}

// TestWebhookRedeliveryWithDeliveryID: the same push sent with the same
// delivery id is answered from the delivery cache and starts nothing.
func TestWebhookRedeliveryWithDeliveryID(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	body := pushBody("refs/heads/main")
	h := githubHeaders("push", hookSign(body))
	h.Set("X-GitHub-Delivery", "delivery-1")

	status, out := f.post(h, body)
	if status != http.StatusAccepted || !strings.Contains(out, `"started"`) {
		t.Fatalf("first delivery = %d %s, want 202 started", status, out)
	}
	first := f.buildNameFrom(t, out)

	status, out = f.post(h, body)
	if status != http.StatusAccepted || !strings.Contains(out, `"duplicate"`) {
		t.Fatalf("redelivery = %d %s, want 202 duplicate", status, out)
	}
	if got := f.buildNameFrom(t, out); got != first {
		t.Fatalf("duplicate names build %q, want %q", got, first)
	}
	if n := len(f.builds()); n != 1 {
		t.Fatalf("%d builds after a redelivery, want 1", n)
	}
}

// TestWebhookNewCommitStartsBuild: a push of a different commit is not a
// duplicate, even though the config already has a Build.
func TestWebhookNewCommitStartsBuild(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	first := pushBody("refs/heads/main")
	if status, out := f.post(githubHeaders("push", hookSign(first)), first); status != http.StatusAccepted || !strings.Contains(out, `"started"`) {
		t.Fatalf("first push = %d %s, want 202 started", status, out)
	}
	second := `{"ref":"refs/heads/main","after":"` + hookTestOtherCommit + `"}`
	status, out := f.post(githubHeaders("push", hookSign(second)), second)
	if status != http.StatusAccepted || !strings.Contains(out, `"started"`) {
		t.Fatalf("push of a new commit = %d %s, want 202 started", status, out)
	}
	if n := len(f.builds()); n != 2 {
		t.Fatalf("%d builds, want 2", n)
	}
}

// buildNameFrom returns the build field of a webhook reply.
func (f *hookFixture) buildNameFrom(t *testing.T, out string) string {
	t.Helper()
	var reply struct {
		Build string `json:"build"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil || reply.Build == "" {
		t.Fatalf("reply %q has no build name", out)
	}
	return reply.Build
}
