// Package registry embeds the CNCF Distribution OCI registry in the server
// (ADR 0020). Authorization is decided here from the client certificate. The
// store is reached only through callbacks, so the package has no store
// dependency and its tests run against a plain TLS server.
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/auth"
	"github.com/distribution/distribution/v3/registry/handlers"

	// Registers the "filesystem" storage driver used by handlers.NewApp.
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"

	"github.com/Shoal-Technology/shoal/internal/security"
)

// TagsPath lists the stored tags as JSON, for admins only. It sits beside
// the OCI API.
const TagsPath = "/shoal/v1/tags"

// Roles a request can have. The empty role is an anonymous request.
const (
	RoleAdmin   = security.RoleAdmin
	RoleNode    = security.RoleNode
	RoleBuilder = security.RoleBuilder
)

const authName = "shoal"

// maxCatalogEntries bounds one page of /v2/_catalog. Distribution reads the
// limit from the parsed config, so a zero value would list nothing.
const maxCatalogEntries = 1000

var manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)

// Principal is the caller of one request.
type Principal struct {
	Role string // RoleAdmin, RoleNode, RoleBuilder, or "" for a request without a client certificate
	Name string
	// Scope is the one repository a builder may use (RoleBuilder only).
	Scope string
}

// Tag is one tag in the store: the manifest digest it points to.
type Tag struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	MediaType  string `json:"mediaType,omitempty"`
}

// Config configures the registry.
type Config struct {
	// Dir holds blobs and manifests (filesystem storage driver).
	Dir string
	// AnonymousPull lets requests without a client certificate pull.
	// Pushes and the catalog always need an admin certificate.
	AnonymousPull bool
	// NodeExists reports whether a Node object with this name exists. A
	// node certificate whose Node was deleted is refused (ADR 0007).
	NodeExists func(ctx context.Context, name string) (bool, error)
	// BuildScope returns the repository of a Build and whether the Build is
	// still running. A builder certificate whose Build has finished, or is
	// unknown, is refused (ADR 0021, pass B). The repository is the scope of
	// the certificate, which must match.
	BuildScope func(ctx context.Context, build string) (repository string, active bool, err error)
	// RecordTag stores a tag once its manifest has been accepted. If it
	// fails the push returns 500 and the client retries.
	RecordTag func(ctx context.Context, t Tag) error
	// ListTags returns the stored tags for TagsPath.
	ListTags func(ctx context.Context) ([]Tag, error)
}

// New returns the registry's HTTP handler. ctx bounds the background work
// of the registry (upload purging).
func New(ctx context.Context, cfg Config) (http.Handler, error) {
	switch {
	case cfg.Dir == "":
		return nil, errors.New("registry: Dir is required")
	case cfg.NodeExists == nil || cfg.BuildScope == nil || cfg.RecordTag == nil || cfg.ListTags == nil:
		return nil, errors.New("registry: NodeExists, BuildScope, RecordTag and ListTags are required")
	}
	secret, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	h := &handler{cfg: cfg}
	ctrl := &controller{anonPull: cfg.AnonymousPull, principal: h.principal}
	conf := &configuration.Configuration{
		Storage: configuration.Storage{
			"filesystem": configuration.Parameters{"rootdirectory": cfg.Dir},
			"delete":     configuration.Parameters{"enabled": false},
		},
		Auth:    configuration.Auth{authName: configuration.Parameters{"controller": ctrl}},
		Catalog: configuration.Catalog{MaxEntries: maxCatalogEntries},
	}
	// The secret signs upload state. A new one per start means uploads in
	// progress at a restart must be retried.
	conf.HTTP.Secret = secret
	h.app = handlers.NewApp(ctx, conf)
	return h, nil
}

type handler struct {
	cfg Config
	app http.Handler
}

// principal reads the caller from the verified client certificate. No
// certificate is an anonymous request. A certificate with an identity that
// the server no longer honours is an error, and the request is refused even
// when anonymous pulls are allowed.
func (h *handler) principal(r *http.Request) (Principal, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Principal{}, nil
	}
	id, err := security.IdentityFromCert(r.TLS.PeerCertificates[0])
	if err != nil {
		return Principal{}, err
	}
	switch id.Role {
	case RoleAdmin:
		return Principal{Role: RoleAdmin, Name: id.Name}, nil
	case RoleNode:
		ok, err := h.cfg.NodeExists(r.Context(), id.Name)
		if err != nil {
			return Principal{}, err
		}
		if !ok {
			return Principal{}, fmt.Errorf("node %q no longer exists", id.Name)
		}
		return Principal{Role: RoleNode, Name: id.Name}, nil
	case RoleBuilder:
		repo, active, err := h.cfg.BuildScope(r.Context(), id.Name)
		if err != nil {
			return Principal{}, err
		}
		if !active || repo != id.Scope {
			return Principal{}, fmt.Errorf("build %q is not running in repository %q", id.Name, id.Scope)
		}
		return Principal{Role: RoleBuilder, Name: id.Name, Scope: repo}, nil
	}
	return Principal{}, fmt.Errorf("unknown role %q", id.Role)
}

// ServeHTTP routes TagsPath to the store and everything else to the
// registry. Manifest pushes that create a tag are recorded before the
// response is sent.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == TagsPath {
		h.serveTags(w, r)
		return
	}
	h.app.ServeHTTP(&tagRecorder{ResponseWriter: w, h: h, r: r}, r)
}

func (h *handler) serveTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, err := h.principal(r)
	if err != nil || p.Role != RoleAdmin {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tags, err := h.cfg.ListTags(r.Context())
	if err != nil {
		http.Error(w, "list tags failed", http.StatusInternalServerError)
		return
	}
	if tags == nil {
		tags = []Tag{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tags)
}

// tagRecorder records the tag of a successful manifest push. The registry
// writes the response status last, so the record is made in WriteHeader.
type tagRecorder struct {
	http.ResponseWriter
	h *handler
	r *http.Request
}

func (t *tagRecorder) WriteHeader(code int) {
	if code == http.StatusCreated && t.r.Method == http.MethodPut {
		if repo, ref, ok := manifestRef(t.r.URL.Path); ok && !strings.Contains(ref, ":") {
			err := t.h.cfg.RecordTag(t.r.Context(), Tag{
				Repository: repo,
				Tag:        ref,
				Digest:     t.Header().Get("Docker-Content-Digest"),
				MediaType:  t.r.Header.Get("Content-Type"),
			})
			if err != nil {
				t.Header().Del("Docker-Content-Digest")
				http.Error(t.ResponseWriter, "image tag not recorded", http.StatusInternalServerError)
				return
			}
		}
	}
	t.ResponseWriter.WriteHeader(code)
}

// Flush passes through to the underlying writer, which blob responses use.
func (t *tagRecorder) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// manifestRef splits /v2/<name>/manifests/<reference>.
func manifestRef(path string) (repo, ref string, ok bool) {
	m := manifestPath.FindStringSubmatch(path)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// controller decides each access the registry asks about.
type controller struct {
	anonPull  bool
	principal func(*http.Request) (Principal, error)
}

// denied refuses a request with 401. It implements auth.Challenge without
// headers: the registry has no password scheme (ADR 0020).
type denied struct{ reason string }

func (d denied) Error() string                               { return "registry: " + d.reason }
func (denied) SetHeaders(*http.Request, http.ResponseWriter) {}

// Authorized implements auth.AccessController.
func (c *controller) Authorized(r *http.Request, access ...auth.Access) (*auth.Grant, error) {
	p, err := c.principal(r)
	if err != nil {
		return nil, denied{"caller not authorized"}
	}
	grant := &auth.Grant{User: auth.UserInfo{Name: p.Name}}
	if p.Role == "" {
		grant.User.Name = "anonymous"
	}
	for _, a := range access {
		if !allowed(p, c.anonPull, a) {
			return nil, denied{"access denied"}
		}
		grant.Resources = append(grant.Resources, a.Resource)
	}
	return grant, nil
}

// allowed is the whole policy (ADR 0020, section 3). Anything not listed is
// denied.
func allowed(p Principal, anonPull bool, a auth.Access) bool {
	switch a.Type {
	case "repository":
		// A builder pulls and pushes its own repository, and no other.
		if p.Role == RoleBuilder {
			return p.Scope != "" && a.Name == p.Scope && (a.Action == "pull" || a.Action == "push" || a.Action == "*")
		}
		switch a.Action {
		case "pull":
			return p.Role == RoleAdmin || p.Role == RoleNode || (p.Role == "" && anonPull)
		case "push", "*":
			return p.Role == RoleAdmin
		}
	case "registry":
		return a.Name == "catalog" && p.Role == RoleAdmin
	}
	return false
}

func init() {
	if err := auth.Register(authName, func(options map[string]any) (auth.AccessController, error) {
		c, ok := options["controller"].(*controller)
		if !ok {
			return nil, errors.New("registry: controller parameter missing")
		}
		return c, nil
	}); err != nil {
		panic(err)
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("registry: random secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}
