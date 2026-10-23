package resource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

const (
	hookSecret = "a-webhook-secret-value"
	hookSHA    = "0123456789abcdef0123456789abcdef01234567"
	hookSHA2   = "fedcba9876543210fedcba9876543210fedcba98"
	zeroSHA    = "0000000000000000000000000000000000000000"
)

// hmacHex is the HMAC-SHA256 of body, keyed with key, in lower case hex.
func hmacHex(key, body string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// hdr returns a header map built with Set, so keys are canonicalised as
// net/http reads them.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func pushJSON(ref, after string) string {
	return `{"ref":"` + ref + `","after":"` + after + `"}`
}

func TestVerifyWebhook(t *testing.T) {
	body := pushJSON("refs/heads/main", hookSHA)
	otherBody := pushJSON("refs/heads/main", hookSHA2)
	sigGitHub := "sha256=" + hmacHex(hookSecret, body)
	cases := []struct {
		name     string
		provider string
		h        http.Header
		body     string
		secret   string
		ok       bool
	}{
		{"github valid", WebhookGitHub, hdr("X-Hub-Signature-256", sigGitHub), body, hookSecret, true},
		{"github replayed on another body", WebhookGitHub, hdr("X-Hub-Signature-256", sigGitHub), otherBody, hookSecret, false},
		{"github wrong secret", WebhookGitHub, hdr("X-Hub-Signature-256", "sha256="+hmacHex("other", body)), body, hookSecret, false},
		{"github missing header", WebhookGitHub, hdr(), body, hookSecret, false},
		{"github not hex", WebhookGitHub, hdr("X-Hub-Signature-256", "sha256=zz"), body, hookSecret, false},
		{"github empty secret", WebhookGitHub, hdr("X-Hub-Signature-256", "sha256="+hmacHex("", body)), body, "", false},
		{"github signature used as token", WebhookGitLab, hdr("X-Gitlab-Token", sigGitHub), body, hookSecret, false},
		{"gitea valid bare hex", WebhookGitea, hdr("X-Gitea-Signature", hmacHex(hookSecret, body)), body, hookSecret, true},
		{"gitea replayed on another body", WebhookGitea, hdr("X-Gitea-Signature", hmacHex(hookSecret, body)), otherBody, hookSecret, false},
		{"gitea github header ignored", WebhookGitea, hdr("X-Hub-Signature-256", sigGitHub), body, hookSecret, false},
		{"generic valid", WebhookGeneric, hdr("X-Shoal-Signature-256", sigGitHub), body, hookSecret, true},
		{"generic replayed on another body", WebhookGeneric, hdr("X-Shoal-Signature-256", sigGitHub), otherBody, hookSecret, false},
		{"generic missing header", WebhookGeneric, hdr(), body, hookSecret, false},
		{"gitlab valid token", WebhookGitLab, hdr("X-Gitlab-Token", hookSecret), body, hookSecret, true},
		{"gitlab wrong token", WebhookGitLab, hdr("X-Gitlab-Token", "nope"), body, hookSecret, false},
		{"gitlab prefix of secret", WebhookGitLab, hdr("X-Gitlab-Token", hookSecret[:5]), body, hookSecret, false},
		{"gitlab missing token", WebhookGitLab, hdr(), body, hookSecret, false},
		{"gitlab empty secret and token", WebhookGitLab, hdr("X-Gitlab-Token", ""), body, "", false},
		{"unknown provider", "bitbucket", hdr("X-Hub-Signature-256", sigGitHub), body, hookSecret, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyWebhook(tc.provider, tc.h, []byte(tc.body), []byte(tc.secret))
			if (err == nil) != tc.ok {
				t.Fatalf("VerifyWebhook() = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrWebhookAuth) {
				t.Fatalf("error = %v, want ErrWebhookAuth", err)
			}
		})
	}
}

func TestParseWebhookEvent(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		h        http.Header
		body     string
		kind     string
		ref      string
		commit   string
		wantErr  bool
	}{
		{"github push", WebhookGitHub, hdr("X-GitHub-Event", "push"), pushJSON("refs/heads/main", hookSHA), WebhookPush, "refs/heads/main", hookSHA, false},
		{"github ping", WebhookGitHub, hdr("X-GitHub-Event", "ping"), `{"zen":"hi"}`, WebhookPing, "", "", false},
		{"github tag create is ignored", WebhookGitHub, hdr("X-GitHub-Event", "create"), `{"ref":"v1"}`, WebhookIgnored, "", "", false},
		{"github deleted branch is ignored", WebhookGitHub, hdr("X-GitHub-Event", "push"), `{"ref":"refs/heads/old","after":"` + zeroSHA + `","deleted":true}`, WebhookIgnored, "", "", false},
		{"github short commit", WebhookGitHub, hdr("X-GitHub-Event", "push"), pushJSON("refs/heads/main", "abc123"), "", "", "", true},
		{"github upper case commit", WebhookGitHub, hdr("X-GitHub-Event", "push"), pushJSON("refs/heads/main", strings.ToUpper(hookSHA)), "", "", "", true},
		{"github invalid json", WebhookGitHub, hdr("X-GitHub-Event", "push"), `{"ref":`, "", "", "", true},
		{"gitlab push hook", WebhookGitLab, hdr("X-Gitlab-Event", "Push Hook"), `{"ref":"refs/heads/main","checkout_sha":"` + hookSHA + `","after":"` + hookSHA + `"}`, WebhookPush, "refs/heads/main", hookSHA, false},
		{"gitlab branch deleted (null checkout)", WebhookGitLab, hdr("X-Gitlab-Event", "Push Hook"), `{"ref":"refs/heads/old","checkout_sha":null,"after":"` + zeroSHA + `"}`, WebhookIgnored, "", "", false},
		{"gitlab merge request is ignored", WebhookGitLab, hdr("X-Gitlab-Event", "Merge Request Hook"), `{}`, WebhookIgnored, "", "", false},
		{"gitea push", WebhookGitea, hdr("X-Gitea-Event", "push"), pushJSON("refs/heads/main", hookSHA), WebhookPush, "refs/heads/main", hookSHA, false},
		{"gitea ping", WebhookGitea, hdr("X-Gitea-Event", "ping"), `{}`, WebhookPing, "", "", false},
		{"generic default event is push", WebhookGeneric, hdr(), `{"ref":"refs/heads/main","commit":"` + hookSHA + `"}`, WebhookPush, "refs/heads/main", hookSHA, false},
		{"generic explicit ping", WebhookGeneric, hdr("X-Shoal-Event", "ping"), `{}`, WebhookPing, "", "", false},
		{"generic commit is required", WebhookGeneric, hdr(), `{"ref":"refs/heads/main"}`, "", "", "", true},
		{"generic 39-character commit", WebhookGeneric, hdr(), `{"ref":"main","commit":"` + hookSHA[:39] + `"}`, "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseWebhookEvent(tc.provider, tc.h, []byte(tc.body))
			if tc.wantErr {
				if !errors.Is(err, ErrWebhookPayload) {
					t.Fatalf("err = %v, want ErrWebhookPayload", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tc.kind || ev.Ref != tc.ref || ev.Commit != tc.commit {
				t.Fatalf("event = %+v, want kind %q ref %q commit %q", ev, tc.kind, tc.ref, tc.commit)
			}
		})
	}
}

func TestWebhookBranchMatch(t *testing.T) {
	cases := []struct {
		ref, filter string
		want        bool
	}{
		{"refs/heads/main", "main", true},
		{"refs/heads/dev", "main", false},
		{"refs/heads/main/extra", "main", false},
		{"refs/tags/main", "main", false},
		{"main", "main", true},
		{"refs/heads/main", "", false},
	}
	for _, tc := range cases {
		if got := WebhookBranchMatch(tc.ref, tc.filter); got != tc.want {
			t.Errorf("WebhookBranchMatch(%q, %q) = %v, want %v", tc.ref, tc.filter, got, tc.want)
		}
	}
}

func TestValidateWebhook(t *testing.T) {
	cases := []struct {
		name string
		w    *v1alpha1.BuildWebhook
		ref  string
		ok   bool
	}{
		{"github with branch", &v1alpha1.BuildWebhook{Provider: "github", Secret: "hooks", Branch: "main"}, "HEAD", true},
		{"branch defaults to a named ref", &v1alpha1.BuildWebhook{Provider: "gitlab", Secret: "hooks"}, "release-1.2", true},
		{"HEAD needs a branch", &v1alpha1.BuildWebhook{Provider: "github", Secret: "hooks"}, "HEAD", false},
		{"commit needs a branch", &v1alpha1.BuildWebhook{Provider: "github", Secret: "hooks"}, hookSHA, false},
		{"unknown provider", &v1alpha1.BuildWebhook{Provider: "bitbucket", Secret: "hooks", Branch: "main"}, "main", false},
		{"no secret", &v1alpha1.BuildWebhook{Provider: "github", Branch: "main"}, "main", false},
		{"bad secret name", &v1alpha1.BuildWebhook{Provider: "github", Secret: "Bad_Name", Branch: "main"}, "main", false},
		{"branch with dots", &v1alpha1.BuildWebhook{Provider: "generic", Secret: "hooks", Branch: "../etc"}, "main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWebhook(tc.w, tc.ref)
			if (err == nil) != tc.ok {
				t.Fatalf("validateWebhook() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
