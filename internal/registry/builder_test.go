package registry

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/security"
)

// issueBuilder issues a builder push certificate for a Build in a repository
// (ADR 0021, pass B).
func issueBuilder(t *testing.T, e *env, build, scope string) tls.Certificate {
	t.Helper()
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := e.ca.IssueBuilder(key.Public(), security.Identity{Name: build, Role: security.RoleBuilder, Scope: scope}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

// TestBuilderScope checks what a builder certificate may do: pull and push its
// own repository while its Build runs, and nothing else.
func TestBuilderScope(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	srv := e.serve(dir, false)
	// A Build's repository is its output.repository, and the scope matches it
	// exactly: the builder pushes "web", not "web/app".
	imagePush(t, e.client(e.admin), srv.URL, "web", "v1", []byte("{}"), []byte("data"))
	imagePush(t, e.client(e.admin), srv.URL, "team/app", "v1", []byte("{}"), []byte("data"))

	e.mu.Lock()
	e.builds["web-abc12"] = buildScope{repo: "web", active: true}
	e.builds["done-xyz34"] = buildScope{repo: "web", active: false}
	e.mu.Unlock()

	builder := e.client(issueBuilder(t, e, "web-abc12", "web"))
	finished := e.client(issueBuilder(t, e, "done-xyz34", "web"))
	wrongScope := e.client(issueBuilder(t, e, "web-abc12", "team"))
	unknown := e.client(issueBuilder(t, e, "gone-000ab", "web"))

	manifest := func(repo string) string { return srv.URL + "/v2/" + repo + "/manifests/v1" }
	pushBlob := func(repo string) string {
		return srv.URL + "/v2/" + repo + "/blobs/uploads/?digest=" + url.QueryEscape(digestOf([]byte("z")))
	}
	cases := []struct {
		name   string
		client *http.Client
		method string
		url    string
		want   int
	}{
		{"builder pulls its repository", builder, http.MethodGet, manifest("web"), http.StatusOK},
		{"builder pushes its repository", builder, http.MethodPost, pushBlob("web"), http.StatusAccepted},
		{"builder cannot push a sub-repository of its scope", builder, http.MethodPost, pushBlob("web/app"), http.StatusUnauthorized},
		{"builder cannot pull another repository", builder, http.MethodGet, manifest("team/app"), http.StatusUnauthorized},
		{"builder cannot push another repository", builder, http.MethodPost, pushBlob("team/app"), http.StatusUnauthorized},
		{"builder cannot read the catalog", builder, http.MethodGet, srv.URL + "/v2/_catalog", http.StatusUnauthorized},
		{"builder cannot read the tags endpoint", builder, http.MethodGet, srv.URL + TagsPath, http.StatusUnauthorized},
		{"finished build loses its certificate", finished, http.MethodGet, manifest("web"), http.StatusUnauthorized},
		{"certificate scoped to another repository is refused", wrongScope, http.MethodGet, manifest("web"), http.StatusUnauthorized},
		{"certificate of an unknown build is refused", unknown, http.MethodGet, manifest("web"), http.StatusUnauthorized},
		{"node certificate still pulls", e.client(e.node), http.MethodGet, manifest("web"), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := do(t, tc.client, tc.method, tc.url, nil, map[string]string{"Accept": ociManifest})
			if status != tc.want {
				t.Fatalf("status %d, want %d", status, tc.want)
			}
		})
	}

	t.Run("a build that finishes loses access at once", func(t *testing.T) {
		e.mu.Lock()
		e.builds["web-abc12"] = buildScope{repo: "web", active: false}
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.builds["web-abc12"] = buildScope{repo: "web", active: true}
			e.mu.Unlock()
		}()
		status, _, _ := do(t, builder, http.MethodGet, manifest("web"), nil, map[string]string{"Accept": ociManifest})
		if status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", status)
		}
	})
}
