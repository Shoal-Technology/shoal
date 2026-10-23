package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

const (
	hookTestSecret = "webhook-test-secret-value"
	hookTestCommit = "0123456789abcdef0123456789abcdef01234567"
)

// hookFixture is a server with a BuildConfig "app" that has a webhook, and its
// Secret "hooks". The portal is served over plain HTTP for the test.
type hookFixture struct {
	t     *testing.T
	ts    *testServer
	admin v1alpha1.ControlClient
	url   string
}

func newHookFixture(t *testing.T, provider string) *hookFixture {
	t.Helper()
	ts := startServer(t, t.TempDir(), &clock{})
	admin := ts.admin(t)
	f := &hookFixture{t: t, ts: ts, admin: admin}
	f.apply("kind: Secret\nmetadata:\n  name: hooks\nspec:\n  data:\n    webhook-secret: " + hookTestSecret + "\n")
	f.apply("kind: BuildConfig\nmetadata:\n  name: app\nspec:\n  git:\n    url: https://github.com/acme/app.git\n    ref: main\n" +
		"  webhook:\n    provider: " + provider + "\n    secret: hooks\n")
	obj, err := admin.Get(context.Background(), &v1alpha1.GetRequest{Kind: v1alpha1.KindBuildConfig, Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	uid := obj.GetBuildConfig().GetMetadata().GetUid()
	if uid == "" {
		t.Fatal("the BuildConfig has no uid")
	}
	srv := httptest.NewServer(ts.PortalHandler())
	t.Cleanup(srv.Close)
	f.url = srv.URL + resource.WebhookPath(uid)
	return f
}

func (f *hookFixture) apply(manifest string) {
	f.t.Helper()
	objs, err := resource.Decode([]byte(manifest))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.admin.Apply(context.Background(), &v1alpha1.ApplyRequest{Objects: objs}); err != nil {
		f.t.Fatal(err)
	}
}

// post sends a webhook request and returns the status and the body.
func (f *hookFixture) post(h http.Header, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header = h
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// builds returns the Builds of config app.
func (f *hookFixture) builds() []*v1alpha1.Build {
	f.t.Helper()
	resp, err := f.admin.List(context.Background(), &v1alpha1.ListRequest{Kind: v1alpha1.KindBuild})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []*v1alpha1.Build
	for _, o := range resp.GetItems() {
		if b := o.GetBuild(); b.GetSpec().GetConfig() == "app" {
			out = append(out, b)
		}
	}
	return out
}

func hookSign(body string) string {
	m := hmac.New(sha256.New, []byte(hookTestSecret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func githubHeaders(event, sig string) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	if sig != "" {
		h.Set("X-Hub-Signature-256", sig)
	}
	return h
}

func pushBody(ref string) string {
	return `{"ref":"` + ref + `","after":"` + hookTestCommit + `"}`
}

func TestWebhookStartsBuildPinnedToCommit(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	body := pushBody("refs/heads/main")
	status, out := f.post(githubHeaders("push", hookSign(body)), body)
	if status != http.StatusAccepted || !strings.Contains(out, `"started"`) {
		t.Fatalf("push = %d %s, want 202 started", status, out)
	}
	bs := f.builds()
	if len(bs) != 1 {
		t.Fatalf("%d builds, want 1", len(bs))
	}
	if got := bs[0].GetSpec().GetTrigger(); got != resource.BuildTriggerWebhook {
		t.Fatalf("trigger = %q, want webhook", got)
	}
	if got := bs[0].GetSpec().GetSource().GetGit().GetRef(); got != hookTestCommit {
		t.Fatalf("build ref = %q, want the pushed commit %s", got, hookTestCommit)
	}
}

func TestWebhookRefusesBadSignatures(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	body := pushBody("refs/heads/main")
	cases := map[string]http.Header{
		"missing signature": githubHeaders("push", ""),
		"wrong signature":   githubHeaders("push", hookSign("another body")),
		"not hex":           githubHeaders("push", "sha256=nothex"),
	}
	for name, h := range cases {
		if status, out := f.post(h, body); status != http.StatusUnauthorized || strings.Contains(out, "signature") {
			t.Errorf("%s: %d %s, want a bare 401", name, status, out)
		}
	}
	if n := len(f.builds()); n != 0 {
		t.Fatalf("%d builds started by refused requests, want 0", n)
	}
}

func TestWebhookBranchFilterAndEvents(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	other := pushBody("refs/heads/feature")
	if status, out := f.post(githubHeaders("push", hookSign(other)), other); status != http.StatusAccepted || !strings.Contains(out, "ignored") {
		t.Fatalf("other branch = %d %s, want 202 ignored", status, out)
	}
	tag := pushBody("refs/tags/main")
	if status, out := f.post(githubHeaders("push", hookSign(tag)), tag); status != http.StatusAccepted || !strings.Contains(out, "ignored") {
		t.Fatalf("tag = %d %s, want 202 ignored", status, out)
	}
	ping := `{"zen":"ok"}`
	if status, _ := f.post(githubHeaders("ping", hookSign(ping)), ping); status != http.StatusOK {
		t.Fatalf("ping = %d, want 200", status)
	}
	if n := len(f.builds()); n != 0 {
		t.Fatalf("%d builds for ignored requests, want 0", n)
	}
}

func TestWebhookSizeLimit(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	body := strings.Repeat("a", resource.MaxWebhookBody+1)
	if status, _ := f.post(githubHeaders("push", hookSign(body)), body); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", status)
	}
}

func TestWebhookUnknownConfigIs404(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	req, err := http.NewRequest(http.MethodPost, f.url[:strings.LastIndex(f.url, "/")+1]+"not-a-uid", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown uid = %d, want 404", resp.StatusCode)
	}
}

func TestWebhookRateLimit(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitHub)
	ping := `{}`
	h := githubHeaders("ping", hookSign(ping))
	for i := range hookRateLimit {
		if status, _ := f.post(h, ping); status != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, status)
		}
	}
	if status, _ := f.post(h, ping); status != http.StatusTooManyRequests {
		t.Fatalf("request %d = %d, want 429", hookRateLimit+1, status)
	}
}

func TestWebhookGitLabToken(t *testing.T) {
	f := newHookFixture(t, resource.WebhookGitLab)
	body := `{"ref":"refs/heads/main","checkout_sha":"` + hookTestCommit + `","after":"` + hookTestCommit + `"}`
	h := http.Header{}
	h.Set("X-Gitlab-Event", "Push Hook")
	h.Set("X-Gitlab-Token", "wrong")
	if status, _ := f.post(h, body); status != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", status)
	}
	h.Set("X-Gitlab-Token", hookTestSecret)
	if status, _ := f.post(h, body); status != http.StatusAccepted {
		t.Fatalf("right token = %d, want 202", status)
	}
	if n := len(f.builds()); n != 1 {
		t.Fatalf("%d builds, want 1", n)
	}
}
