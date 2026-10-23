package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// Git webhooks (ADR 0021, pass C). The portal serves POST /hooks/v1/<config
// uid>. The uid is random, so the URL is not guessable, and the request must
// also carry the config's secret. Nothing is said to a caller about why a
// request was refused, and the secret and the body are never logged.

// hookRateLimit is how many webhook requests one config accepts per window.
const (
	hookRateLimit  = 30
	hookRateWindow = time.Minute

	// hookDeliveryCacheSize is how many webhook deliveries are remembered, so a
	// redelivery of one is answered without reading the store (issue #13).
	hookDeliveryCacheSize = 1024
)

// hook answers a git host's webhook request for one BuildConfig.
func (p *portal) hook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := r.PathValue("uid")
	rec, found, err := p.s.buildConfigByUID(ctx, uid)
	if err != nil {
		p.s.log.Error("webhook: look up build config", "err", err)
		hookReply(w, http.StatusInternalServerError, `{"error":"internal error"}`)
		return
	}
	if !found {
		hookReply(w, http.StatusNotFound, `{"error":"not found"}`)
		return
	}
	cfg, err := objects.BuildConfig(rec)
	if err != nil {
		p.s.log.Error("webhook: decode build config", "config", rec.Name, "err", err)
		hookReply(w, http.StatusInternalServerError, `{"error":"internal error"}`)
		return
	}
	hook := cfg.GetSpec().GetWebhook()
	if hook == nil {
		hookReply(w, http.StatusNotFound, `{"error":"not found"}`)
		return
	}
	if !p.allowHook(uid) {
		w.Header().Set("Retry-After", "60")
		hookReply(w, http.StatusTooManyRequests, `{"error":"too many requests"}`)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, resource.MaxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			hookReply(w, http.StatusRequestEntityTooLarge, `{"error":"payload too large"}`)
			return
		}
		hookReply(w, http.StatusBadRequest, `{"error":"bad request"}`)
		return
	}
	secret, err := p.s.webhookSecret(ctx, hook.GetSecret())
	if err != nil {
		// Without the secret nothing can match. The reason is logged, not sent.
		p.s.log.Warn("webhook: secret unusable", "config", rec.Name, "secret", hook.GetSecret(), "err", err)
		hookReply(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		return
	}
	if err := resource.VerifyWebhook(hook.GetProvider(), r.Header, body, secret); err != nil {
		p.s.log.Warn("webhook: request not signed with the secret", "config", rec.Name, "provider", hook.GetProvider())
		hookReply(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		return
	}
	ev, err := resource.ParseWebhookEvent(hook.GetProvider(), r.Header, body)
	if err != nil {
		hookReply(w, http.StatusBadRequest, `{"error":"not a push event"}`)
		return
	}
	switch {
	case ev.Kind == resource.WebhookPing:
		hookReply(w, http.StatusOK, `{"status":"pong"}`)
		return
	case ev.Kind != resource.WebhookPush, !resource.WebhookBranchMatch(ev.Ref, resource.WebhookBranch(cfg.GetSpec())):
		hookReply(w, http.StatusAccepted, `{"status":"ignored"}`)
		return
	}
	// A redelivery of a delivery already handled gets the Build it started.
	deliveryKey := ""
	if id := resource.WebhookDeliveryID(r.Header); id != "" {
		deliveryKey = rec.UID + " " + id
		if build, ok := p.deliveries.Get(deliveryKey); ok {
			p.s.log.Info("webhook: delivery already handled", "config", rec.Name, "build", build)
			hookReply(w, http.StatusAccepted, fmt.Sprintf(`{"status":"duplicate","build":%q}`, build))
			return
		}
	}
	name, dup, err := p.s.startWebhookBuild(ctx, cfg.GetSpec(), rec.Name, ev.Commit)
	if err != nil {
		p.s.log.Error("webhook: build not started", "config", rec.Name, "err", err)
		hookReply(w, http.StatusInternalServerError, `{"error":"build not started"}`)
		return
	}
	if deliveryKey != "" {
		p.deliveries.Put(deliveryKey, name)
	}
	if dup {
		p.s.log.Info("webhook: commit already built", "build", name, "config", rec.Name, "commit", ev.Commit)
		hookReply(w, http.StatusAccepted, fmt.Sprintf(`{"status":"duplicate","build":%q}`, name))
		return
	}
	p.s.log.Info("build started by webhook", "build", name, "config", rec.Name, "commit", ev.Commit)
	hookReply(w, http.StatusAccepted, fmt.Sprintf(`{"status":"started","build":%q}`, name))
}

// startWebhookBuild starts a Build of config pinned to commit: the Build's
// source names the commit instead of the branch, so the run builds exactly
// what was pushed. If a Build of config already covers the commit, nothing is
// started: dup is true and name is that existing Build. The check and the
// create run under hookMu, so two deliveries of one push cannot both start a
// Build.
func (s *Server) startWebhookBuild(ctx context.Context, cfg *v1alpha1.BuildConfigSpec, config, commit string) (name string, dup bool, err error) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	builds, err := s.buildsOf(ctx)
	if err != nil {
		return "", false, err
	}
	if existing, ok := resource.FindDuplicateBuild(config, commit, builds); ok {
		return existing, true, nil
	}
	if secret := cfg.GetCredentialsSecret(); secret != "" {
		if err := s.checkBuildCredentials(ctx, cfg.GetGit().GetUrl(), secret); err != nil {
			return "", false, err
		}
	}
	src := proto.CloneOf(cfg)
	src.Git.Ref = commit
	name, err = s.createBuild(ctx, config, src, resource.BuildTriggerWebhook)
	return name, false, err
}

// buildsOf returns the Builds in the store. A Build that cannot be decoded is
// logged and skipped: it cannot be matched, and it must not stop a push.
func (s *Server) buildsOf(ctx context.Context) ([]*v1alpha1.Build, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindBuild)
	if err != nil {
		return nil, apiError(err)
	}
	out := make([]*v1alpha1.Build, 0, len(recs))
	for _, rec := range recs {
		b, err := objects.Build(rec)
		if err != nil {
			s.log.Warn("webhook: skip undecodable build", "build", rec.Name, "err", err)
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// buildConfigByUID returns the BuildConfig with the uid, if there is one.
func (s *Server) buildConfigByUID(ctx context.Context, uid string) (store.Record, bool, error) {
	if uid == "" {
		return store.Record{}, false, nil
	}
	recs, err := s.store.List(ctx, v1alpha1.KindBuildConfig)
	if err != nil {
		return store.Record{}, false, err
	}
	for _, r := range recs {
		if r.UID == uid {
			return r, true, nil
		}
	}
	return store.Record{}, false, nil
}

// webhookSecret returns the shared secret of a webhook: the webhook-secret key
// of an Opaque Secret. An empty value is an error, so an empty secret never
// accepts a request.
func (s *Server) webhookSecret(ctx context.Context, name string) ([]byte, error) {
	rec, err := s.store.Get(ctx, v1alpha1.KindSecret, name)
	if err != nil {
		return nil, err
	}
	sec, err := objects.Secret(rec)
	if err != nil {
		return nil, err
	}
	if t := sec.GetSpec().GetType(); t != "" && t != resource.SecretTypeOpaque {
		return nil, fmt.Errorf("secret %s has type %s, want Opaque", name, t)
	}
	v := sec.GetSpec().GetData()[resource.WebhookSecretKey]
	if v == "" {
		return nil, fmt.Errorf("secret %s has no %s key", name, resource.WebhookSecretKey)
	}
	return []byte(v), nil
}

// allowHook counts a webhook request against its config and reports whether
// the config is still under hookRateLimit for the window. It is called only
// for a config that exists, so the map holds one entry per config.
func (p *portal) allowHook(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	keep := p.hookHits[uid][:0]
	for _, t := range p.hookHits[uid] {
		if now.Sub(t) < hookRateWindow {
			keep = append(keep, t)
		}
	}
	allowed := len(keep) < hookRateLimit
	if allowed {
		keep = append(keep, now)
	}
	p.hookHits[uid] = keep
	return allowed
}

func hookReply(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}
