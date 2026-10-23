package resource

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Git webhooks (ADR 0021, pass C; parity OB-11, OB-12, GL-02). A git host
// posts to /hooks/v1/<BuildConfig uid> on the portal. The request is checked
// with the config's secret before its body is read as an event.
const (
	WebhookGitHub  = "github"
	WebhookGitLab  = "gitlab"
	WebhookGitea   = "gitea"
	WebhookGeneric = "generic"

	// WebhookSecretKey is the key, in an Opaque Secret, that holds the shared
	// secret of a webhook.
	WebhookSecretKey = "webhook-secret"
	// MaxWebhookBody is the largest request body a webhook accepts.
	MaxWebhookBody = 1 << 20

	// BuildTriggerWebhook is the trigger of a Build started by a push.
	BuildTriggerWebhook = "webhook"
)

// Event kinds a webhook request can carry.
const (
	WebhookPing    = "ping"    // the host checks the URL: answer 200
	WebhookPush    = "push"    // a branch or tag moved to Commit
	WebhookIgnored = "ignored" // any other event, or a deleted ref
)

// ErrWebhookAuth means the request is not signed with the config's secret. The
// caller answers 401 and says nothing more.
var ErrWebhookAuth = errors.New("webhook: signature or token does not match")

// ErrWebhookPayload means the body of a push is not what the provider sends.
var ErrWebhookPayload = errors.New("webhook: payload is not a push event")

// WebhookEvent is a request that passed VerifyWebhook.
type WebhookEvent struct {
	Kind   string // WebhookPing, WebhookPush or WebhookIgnored
	Ref    string // the ref that moved, as sent (refs/heads/main)
	Commit string // the commit the ref now points at, lowercase hex
	// ChangedPaths are the files the push changed, from the commits it carries
	// (ADR 0022, section 2). A large push may send only some of its commits.
	ChangedPaths []string
}

// VerifyWebhook checks the signature or token of a request against the shared
// secret. GitHub, Gitea and generic webhooks send an HMAC-SHA256 of the body;
// GitLab sends the secret itself as a token. Every comparison is constant
// time, and an empty secret or a missing header never matches.
func VerifyWebhook(provider string, h http.Header, body, secret []byte) error {
	if len(secret) == 0 {
		return ErrWebhookAuth
	}
	switch provider {
	case WebhookGitHub:
		return verifyHMAC(h.Get("X-Hub-Signature-256"), body, secret)
	case WebhookGitea:
		return verifyHMAC(h.Get("X-Gitea-Signature"), body, secret)
	case WebhookGeneric:
		return verifyHMAC(h.Get("X-Shoal-Signature-256"), body, secret)
	case WebhookGitLab:
		return verifyToken(h.Get("X-Gitlab-Token"), secret)
	}
	return ErrWebhookAuth
}

// verifyHMAC compares a hex HMAC-SHA256 of body, optionally written as
// "sha256=<hex>", with the signature header.
func verifyHMAC(sig string, body, secret []byte) error {
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil || len(got) == 0 {
		return ErrWebhookAuth
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrWebhookAuth
	}
	return nil
}

// verifyToken compares a token header with the secret. Both are hashed first,
// so the comparison takes the same time whatever their lengths.
func verifyToken(tok string, secret []byte) error {
	if tok == "" {
		return ErrWebhookAuth
	}
	a := sha256.Sum256([]byte(tok))
	b := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		return ErrWebhookAuth
	}
	return nil
}

// ParseWebhookEvent reads the event of a request that passed VerifyWebhook.
// Events that are not pushes are WebhookIgnored, and so is a push that deletes
// its ref. A push with a malformed body is ErrWebhookPayload.
func ParseWebhookEvent(provider string, h http.Header, body []byte) (WebhookEvent, error) {
	ignored := WebhookEvent{Kind: WebhookIgnored}
	ping := WebhookEvent{Kind: WebhookPing}
	switch provider {
	case WebhookGitHub:
		switch h.Get("X-GitHub-Event") {
		case "ping":
			return ping, nil
		case "push":
			return parsePush(body)
		}
	case WebhookGitea:
		switch h.Get("X-Gitea-Event") {
		case "ping":
			return ping, nil
		case "push":
			return parsePush(body)
		}
	case WebhookGitLab:
		if h.Get("X-Gitlab-Event") == "Push Hook" {
			return parsePush(body)
		}
	case WebhookGeneric:
		switch strings.ToLower(h.Get("X-Shoal-Event")) {
		case "", "push":
			return parsePush(body)
		case "ping":
			return ping, nil
		}
	}
	return ignored, nil
}

// pushPayload holds the fields of a push that the webhook uses. GitHub and
// Gitea send after; GitLab sends checkout_sha (null when the branch is
// deleted) and after; a generic sender sends commit.
type pushPayload struct {
	Ref         string `json:"ref"`
	After       string `json:"after"`
	CheckoutSHA string `json:"checkout_sha"`
	Commit      string `json:"commit"`
	Deleted     bool   `json:"deleted"`
	Commits     []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
}

// maxChangedPaths bounds the paths a push event carries into a run.
const maxChangedPaths = 10000

func parsePush(body []byte) (WebhookEvent, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return WebhookEvent{}, ErrWebhookPayload
	}
	ignored := WebhookEvent{Kind: WebhookIgnored}
	commit := firstNonEmpty(p.Commit, p.CheckoutSHA, p.After)
	if p.Deleted || isZeroSHA(commit) {
		return ignored, nil
	}
	if !isFullSHA(commit) || p.Ref == "" || len(p.Ref) > 255 {
		return WebhookEvent{}, ErrWebhookPayload
	}
	seen := map[string]bool{}
	var changed []string
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if !seen[f] && len(changed) < maxChangedPaths {
					seen[f] = true
					changed = append(changed, f)
				}
			}
		}
	}
	return WebhookEvent{Kind: WebhookPush, Ref: p.Ref, Commit: commit, ChangedPaths: changed}, nil
}

// WebhookBranch is the branch whose pushes start a Build: the trigger's own
// branch, or the config's ref when none is set.
func WebhookBranch(s *v1alpha1.BuildConfigSpec) string {
	if b := s.GetWebhook().GetBranch(); b != "" {
		return b
	}
	return s.GetGit().GetRef()
}

// WebhookBranchMatch reports whether a pushed ref is the filter branch. A
// push of a tag never matches, and a bare branch name (as a generic sender
// may send) matches by name.
func WebhookBranchMatch(ref, filter string) bool {
	if filter == "" {
		return false
	}
	name, isBranch := strings.CutPrefix(ref, "refs/heads/")
	if !isBranch {
		if strings.HasPrefix(ref, "refs/") {
			return false
		}
		name = ref
	}
	return name == filter
}

// validateWebhook checks the webhook trigger of a config. ref is the config's
// git ref. A branch is required when the ref does not name one (HEAD, or a
// commit), because no push would ever match it.
func validateWebhook(w *v1alpha1.BuildWebhook, ref string) error {
	var errs []error
	switch w.GetProvider() {
	case WebhookGitHub, WebhookGitLab, WebhookGitea, WebhookGeneric:
	default:
		errs = append(errs, fmt.Errorf("spec.webhook.provider: %q must be github, gitlab, gitea or generic", w.GetProvider()))
	}
	if s := w.GetSecret(); s == "" {
		errs = append(errs, errors.New("spec.webhook.secret: required (a Secret with the key "+WebhookSecretKey+")"))
	} else if !ValidName(s, 63) {
		errs = append(errs, fmt.Errorf("spec.webhook.secret: invalid Secret name %q", s))
	}
	if b := w.GetBranch(); b != "" {
		if !validGitRef(b) {
			errs = append(errs, fmt.Errorf("spec.webhook.branch: %q is not a branch name", b))
		}
	} else if ref == DefaultGitRef || isFullSHA(ref) {
		errs = append(errs, errors.New("spec.webhook.branch: required when spec.git.ref is HEAD or a commit"))
	}
	return errors.Join(errs...)
}

// WebhookPath is the portal path of a config's webhook.
func WebhookPath(uid string) string { return "/hooks/v1/" + uid }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// isFullSHA reports whether s is a full SHA-1 or SHA-256 commit id in lower
// case hex.
func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isZeroSHA(s string) bool {
	return s != "" && strings.Trim(s, "0") == ""
}
