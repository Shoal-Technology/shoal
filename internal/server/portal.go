package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	portalfs "example.com/shoal/internal/portal"
	"example.com/shoal/internal/resource"
	"example.com/shoal/internal/security"
	"example.com/shoal/internal/store"
)

// Web portal settings.
const (
	sessionCookie  = "shoal_session"
	sessionTTL     = 12 * time.Hour // sliding: extended on every request
	loginWindow    = 5 * time.Minute
	loginFailures  = 5 // per IP and window, then logins are refused
	csrfHeader     = "X-Shoal-Request"
	uiAuthName     = "admin"
	maxRequestBody = 1 << 20
)

// UIInitialPasswordPath holds the portal password generated on first start
// until it is changed with `shoal ui reset-password`.
func UIInitialPasswordPath(dataDir string) string {
	return filepath.Join(dataDir, "ui-initial-password")
}

// portal serves the web UI and its JSON API. It calls the Control API
// in-process, so the portal and the CLI behave the same.
type portal struct {
	s   *Server
	ctl *controlService

	mu       sync.Mutex
	sessions map[string]time.Time   // sha256(token) -> expiry
	failures map[string][]time.Time // client IP -> recent failed logins
}

// ensureUIPassword creates the portal's admin password on first start. It
// is written to a root-only file, never to the log ("no secrets in logs").
func (s *Server) ensureUIPassword(ctx context.Context) error {
	if _, err := s.store.Get(ctx, v1alpha1.KindUIAuth, uiAuthName); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Only the leader creates the password. A follower can be past
	// WaitReady and still miss the leader's record, because its commit index
	// is learned from heartbeats and can trail the log. It reads the
	// replicated record later instead.
	if s.raft != nil && !s.raft.IsLeader() {
		return nil
	}
	pw, err := newPassword()
	if err != nil {
		return fmt.Errorf("startup: generate portal password: %w", err)
	}
	if err := s.setUIPassword(ctx, pw); err != nil {
		if isNotLeader(err) {
			// Leadership moved during startup. The new leader creates it.
			return nil
		}
		return fmt.Errorf("startup: create portal password: %w", err)
	}
	path := UIInitialPasswordPath(s.cfg.DataDir)
	if err := security.WriteFileAtomic(path, []byte(pw+"\n"), 0o600); err != nil {
		return err
	}
	s.log.Info("generated web portal password", "user", "admin", "passwordFile", path)
	return nil
}

func newPassword() (string, error) {
	// Four groups of five from the token alphabet: about 103 bits.
	var parts []string
	for range 4 {
		id, _, err := security.NewToken()
		if err != nil {
			return "", err
		}
		parts = append(parts, id[:5])
	}
	return strings.Join(parts, "-"), nil
}

func (s *Server) setUIPassword(ctx context.Context, pw string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	spec, err := encode(&v1alpha1.UIAuthSpec{PasswordHash: hash, ChangedAt: ts(s.now())})
	if err != nil {
		return err
	}
	for range casRetries {
		rec, err := s.store.Get(ctx, v1alpha1.KindUIAuth, uiAuthName)
		if errors.Is(err, store.ErrNotFound) {
			_, err = s.store.Create(ctx, store.Record{Kind: v1alpha1.KindUIAuth, Name: uiAuthName, Spec: spec})
			if errors.Is(err, store.ErrExists) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		if _, err = s.store.Update(ctx, v1alpha1.KindUIAuth, uiAuthName, spec, rec.Generation); errors.Is(err, store.ErrConflict) {
			continue
		}
		return err
	}
	return status.Error(codes.Aborted, "password changed concurrently, try again")
}

// ResetUIPassword sets a new random portal password and logs everyone out.
func (c *controlService) ResetUIPassword(ctx context.Context, _ *v1alpha1.ResetUIPasswordRequest) (*v1alpha1.ResetUIPasswordResponse, error) {
	pw, err := newPassword()
	if err != nil {
		return nil, apiError(err)
	}
	if err := c.s.setUIPassword(ctx, pw); err != nil {
		return nil, apiError(err)
	}
	_ = os.Remove(UIInitialPasswordPath(c.s.cfg.DataDir)) // stale now
	if p := c.s.portal; p != nil {
		p.mu.Lock()
		clear(p.sessions)
		p.mu.Unlock()
	}
	c.s.log.Info("web portal password reset")
	return &v1alpha1.ResetUIPasswordResponse{Password: pw}, nil
}

// PortalHandler returns the web portal: static UI and JSON API.
func (s *Server) PortalHandler() http.Handler {
	if s.portal == nil {
		s.portal = &portal{s: s, ctl: &controlService{s: s}, sessions: map[string]time.Time{}, failures: map[string][]time.Time{}}
	}
	p := s.portal
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", p.login)
	mux.HandleFunc("POST /api/logout", p.logout)
	mux.HandleFunc("GET /api/me", p.me)
	mux.HandleFunc("GET /ca.crt", p.caCert)
	mux.Handle("GET /api/objects/{kind}", p.auth(p.list))
	mux.Handle("GET /api/objects/{kind}/{name}", p.auth(p.get))
	mux.Handle("GET /api/objects/{kind}/{name}/yaml", p.auth(p.yaml))
	mux.Handle("DELETE /api/objects/{kind}/{name}", p.auth(p.delete))
	mux.Handle("POST /api/apply", p.auth(p.apply))
	mux.Handle("POST /api/workloads/{name}/restart", p.auth(p.restart))
	mux.Handle("POST /api/workloads/{name}/scale", p.auth(p.scale))
	mux.Handle("PATCH /api/data/{kind}/{name}", p.auth(p.patchData))
	mux.Handle("POST /api/tokens", p.auth(p.token))
	mux.Handle("GET /api/logs", p.auth(p.logs))
	static, _ := fs.Sub(portalfs.Static, "static")
	mux.Handle("GET /", http.FileServerFS(static))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (p *portal) login(w http.ResponseWriter, r *http.Request) {
	ip, now := clientIP(r), p.s.now()
	p.mu.Lock()
	var recent []time.Time
	for _, t := range p.failures[ip] {
		if now.Sub(t) < loginWindow {
			recent = append(recent, t)
		}
	}
	p.failures[ip] = recent
	blocked := len(recent) >= loginFailures
	p.mu.Unlock()
	if blocked {
		writeError(w, http.StatusTooManyRequests, "too many failed logins, try again in a few minutes")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	rec, err := p.s.store.Get(r.Context(), v1alpha1.KindUIAuth, uiAuthName)
	ok := false
	if err == nil {
		spec := &v1alpha1.UIAuthSpec{}
		if proto.Unmarshal(rec.Spec, spec) == nil {
			ok = bcrypt.CompareHashAndPassword(spec.GetPasswordHash(), []byte(req.Password)) == nil
		}
	}
	if !ok {
		p.mu.Lock()
		p.failures[ip] = append(p.failures[ip], now)
		p.mu.Unlock()
		p.s.log.Warn("web portal login failed", "ip", ip)
		time.Sleep(500 * time.Millisecond) // slows guessing further
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // never fails (crypto/rand panics instead)
	token := hex.EncodeToString(raw[:])
	p.mu.Lock()
	p.sessions[hashToken(token)] = now.Add(sessionTTL)
	delete(p.failures, ip)
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	p.s.log.Info("web portal login", "ip", ip)
	w.WriteHeader(http.StatusNoContent)
}

func (p *portal) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		p.mu.Lock()
		delete(p.sessions, hashToken(c.Value))
		p.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// session reports whether the request carries a valid session, extending it.
func (p *portal) session(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	key, now := hashToken(c.Value), p.s.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.sessions[key]
	if !ok || now.After(exp) {
		delete(p.sessions, key)
		return false
	}
	p.sessions[key] = now.Add(sessionTTL)
	return true
}

func (p *portal) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"authenticated": p.session(r), "server": p.s.cfg.AdvertiseAddress})
}

func (p *portal) caCert(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="shoal-ca.crt"`)
	_, _ = w.Write(security.EncodeCertPEM(p.s.ca.Cert))
}

// auth requires a session, and for changes also the CSRF header, which
// other sites can't set on cross-origin requests.
func (p *portal) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.session(r) {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		h(w, r)
	})
}

var urlKinds = map[string]string{
	"workloads": v1alpha1.KindWorkload, "replicas": v1alpha1.KindReplica, "nodes": v1alpha1.KindNode,
	"services": v1alpha1.KindService, "routes": v1alpha1.KindRoute, "secrets": v1alpha1.KindSecret, "configs": v1alpha1.KindConfig,
}

func kindOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	k, ok := urlKinds[r.PathValue("kind")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown kind")
	}
	return k, ok
}

var jsonOpts = protojson.MarshalOptions{EmitUnpopulated: false}

func (p *portal) list(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	resp, err := p.ctl.List(r.Context(), &v1alpha1.ListRequest{Kind: kind})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	items := make([]json.RawMessage, 0, len(resp.GetItems()))
	for _, o := range resp.GetItems() {
		b, err := jsonOpts.Marshal(resource.Inner(o))
		if err != nil {
			p.writeRPCError(w, err)
			return
		}
		items = append(items, b)
	}
	writeJSON(w, map[string]any{"items": items})
}

func (p *portal) get(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	obj, err := p.ctl.Get(r.Context(), &v1alpha1.GetRequest{Kind: kind, Name: r.PathValue("name")})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	b, err := jsonOpts.Marshal(resource.Inner(obj))
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// yaml returns an object as an editable manifest: kind, name and spec.
func (p *portal) yaml(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	if kind == v1alpha1.KindSecret {
		writeError(w, http.StatusBadRequest, "secret values are never shown; set them in the form instead")
		return
	}
	obj, err := p.ctl.Get(r.Context(), &v1alpha1.GetRequest{Kind: kind, Name: r.PathValue("name")})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	meta := &v1alpha1.ObjectMeta{Name: r.PathValue("name")}
	var m proto.Message
	switch o := resource.Inner(obj).(type) {
	case *v1alpha1.Workload:
		m = &v1alpha1.Workload{ApiVersion: o.ApiVersion, Kind: o.Kind, Metadata: meta, Spec: o.Spec}
	case *v1alpha1.Service:
		m = &v1alpha1.Service{ApiVersion: o.ApiVersion, Kind: o.Kind, Metadata: meta, Spec: o.Spec}
	case *v1alpha1.Config:
		m = &v1alpha1.Config{ApiVersion: o.ApiVersion, Kind: o.Kind, Metadata: meta, Spec: o.Spec}
	default:
		writeError(w, http.StatusBadRequest, "this kind can't be edited as YAML")
		return
	}
	y, err := resource.EncodeYAML(m)
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	_, _ = w.Write(y)
}

func (p *portal) delete(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	if _, err := p.ctl.Delete(r.Context(), &v1alpha1.DeleteRequest{Kind: kind, Name: r.PathValue("name")}); err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apply takes a YAML (or JSON) manifest, like `shoal apply -f`.
func (p *portal) apply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "manifest too large")
		return
	}
	objs, err := resource.Decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := p.ctl.Apply(r.Context(), &v1alpha1.ApplyRequest{Objects: objs})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	var results []map[string]string
	for _, res := range resp.GetResults() {
		results = append(results, map[string]string{"kind": res.Kind, "name": res.Name, "action": res.Action})
	}
	writeJSON(w, map[string]any{"results": results})
}

func (p *portal) restart(w http.ResponseWriter, r *http.Request) {
	if _, err := p.ctl.RestartWorkload(r.Context(), &v1alpha1.RestartWorkloadRequest{Name: r.PathValue("name")}); err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *portal) scale(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Replicas *int32 `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Replicas == nil {
		writeError(w, http.StatusBadRequest, `want {"replicas": N}`)
		return
	}
	err := p.s.updateWorkloadSpec(r.Context(), r.PathValue("name"), func(s *v1alpha1.WorkloadSpec) error {
		s.Replicas = req.Replicas
		w := &v1alpha1.Workload{Metadata: &v1alpha1.ObjectMeta{Name: r.PathValue("name")}, Spec: s}
		return resource.ValidateWorkload(w)
	})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchData sets and removes keys of a Secret or Config (creating it if
// needed). Secrets can only be changed this way in the portal, since their
// values are never sent back.
func (p *portal) patchData(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok || (kind != v1alpha1.KindSecret && kind != v1alpha1.KindConfig) {
		if ok {
			writeError(w, http.StatusBadRequest, "only secrets and configs have data")
		}
		return
	}
	var req struct {
		Set    map[string]string `json:"set"`
		Remove []string          `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	name := r.PathValue("name")
	data := map[string]string{}
	rec, err := p.s.store.Get(r.Context(), kind, name)
	switch {
	case err == nil && kind == v1alpha1.KindSecret:
		s, err := objects.Secret(rec)
		if err != nil {
			p.writeRPCError(w, err)
			return
		}
		data = s.Spec.GetData()
	case err == nil:
		c, err := objects.Config(rec)
		if err != nil {
			p.writeRPCError(w, err)
			return
		}
		data = c.Spec.GetData()
	case !errors.Is(err, store.ErrNotFound):
		p.writeRPCError(w, err)
		return
	}
	if data == nil {
		data = map[string]string{}
	}
	for k, v := range req.Set {
		data[k] = v
	}
	for _, k := range req.Remove {
		delete(data, k)
	}
	obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Config{Config: &v1alpha1.Config{Metadata: &v1alpha1.ObjectMeta{Name: name}, Spec: &v1alpha1.ConfigSpec{Data: data}}}}
	if kind == v1alpha1.KindSecret {
		obj = &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: &v1alpha1.Secret{Metadata: &v1alpha1.ObjectMeta{Name: name}, Spec: &v1alpha1.SecretSpec{Data: data}}}}
	}
	if _, err := p.ctl.Apply(r.Context(), &v1alpha1.ApplyRequest{Objects: []*v1alpha1.Object{obj}}); err != nil {
		p.writeRPCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *portal) token(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TTLHours int   `json:"ttlHours"`
		MaxUses  int32 `json:"maxUses"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.TTLHours <= 0 {
		req.TTLHours = 24
	}
	resp, err := p.ctl.CreateJoinToken(r.Context(), &v1alpha1.CreateJoinTokenRequest{
		Ttl: durationpb.New(time.Duration(req.TTLHours) * time.Hour), MaxUses: req.MaxUses, Description: "web portal",
	})
	if err != nil {
		p.writeRPCError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"command":   fmt.Sprintf("sudo shoal agent --server %s --token %s --ca-hash %s", p.s.cfg.AdvertiseAddress, resp.Token, resp.CaHash),
		"expiresAt": resp.ExpiresAt.AsTime(),
	})
}

// logs streams log lines as server-sent events.
func (p *portal) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tail, err := strconv.Atoi(q.Get("tail"))
	if err != nil {
		tail = 200
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	stream := &sseLogStream{ctx: r.Context(), w: w, flush: flusher.Flush}
	err = p.ctl.Logs(&v1alpha1.LogsRequest{
		Workload: q.Get("workload"), Replica: q.Get("replica"), Follow: q.Get("follow") == "1", Tail: int32(tail),
	}, stream)
	if err != nil && r.Context().Err() == nil {
		fmt.Fprintf(w, "event: failure\ndata: %s\n\n", strings.ReplaceAll(status.Convert(err).Message(), "\n", " "))
	}
	fmt.Fprint(w, "event: end\ndata: {}\n\n")
	flusher.Flush()
}

// sseLogStream adapts the Logs RPC's stream to server-sent events.
type sseLogStream struct {
	ctx   context.Context
	w     io.Writer
	flush func()
}

func (s *sseLogStream) Send(l *v1alpha1.LogLine) error {
	b, err := protojson.Marshal(l)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.flush()
	return nil
}

func (s *sseLogStream) Context() context.Context     { return s.ctx }
func (s *sseLogStream) SetHeader(metadata.MD) error  { return nil }
func (s *sseLogStream) SendHeader(metadata.MD) error { return nil }
func (s *sseLogStream) SetTrailer(metadata.MD)       {}
func (s *sseLogStream) SendMsg(m any) error          { return s.Send(m.(*v1alpha1.LogLine)) }
func (s *sseLogStream) RecvMsg(any) error            { return io.EOF }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeRPCError answers a failed control call. A write that reached a
// follower gets 503 with the leader's API address in the message, so a
// client can retry there (ADR 0014).
func (p *portal) writeRPCError(w http.ResponseWriter, err error) {
	if isNotLeader(err) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		writeError(w, http.StatusServiceUnavailable, status.Convert(p.s.notLeader(ctx)).Message())
		return
	}
	st := status.Convert(apiError(err))
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		code = http.StatusConflict
	case codes.Unauthenticated:
		code = http.StatusUnauthorized
	case codes.PermissionDenied:
		code = http.StatusForbidden
	case codes.ResourceExhausted:
		code = http.StatusTooManyRequests
	}
	writeError(w, code, st.Message())
}

// updateWorkloadSpec changes a workload's spec with a compare-and-swap
// retry loop; change may reject the result.
func (s *Server) updateWorkloadSpec(ctx context.Context, name string, change func(*v1alpha1.WorkloadSpec) error) error {
	for range casRetries {
		rec, err := s.store.Get(ctx, v1alpha1.KindWorkload, name)
		if err != nil {
			return err
		}
		w, err := objects.Workload(rec)
		if err != nil {
			return err
		}
		if err := change(w.Spec); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		b, err := encode(w.Spec)
		if err != nil {
			return err
		}
		if _, err = s.store.Update(ctx, v1alpha1.KindWorkload, name, b, rec.Generation); errors.Is(err, store.ErrConflict) {
			continue
		}
		return err
	}
	return status.Error(codes.Aborted, "too many concurrent changes, try again")
}

// PortalTLS returns a TLS config for the portal with a certificate from the
// cluster CA for the server's names and addresses (browsers will warn
// until the CA is trusted; see /ca.crt) or, if certFile is set, the
// user's own certificate.
func (s *Server) PortalTLS(certFile, keyFile string, extraNames []string) (*tls.Config, error) {
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("portal certificate: %w", err)
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
	}
	names := append([]string{"localhost"}, extraNames...)
	if h, err := os.Hostname(); err == nil {
		names = append(names, h)
	}
	if host, _, err := net.SplitHostPort(s.cfg.AdvertiseAddress); err == nil {
		names = append(names, host)
	}
	var dns []string
	var ips []net.IP
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
		} else if n != "" {
			dns = append(dns, n)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				ips = append(ips, n.IP)
			}
		}
	}
	key, err := security.NewKey()
	if err != nil {
		return nil, err
	}
	leaf, err := s.ca.IssueServerFor(key.Public(), s.now(), dns, ips)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12, // browsers
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, s.ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}},
	}, nil
}
