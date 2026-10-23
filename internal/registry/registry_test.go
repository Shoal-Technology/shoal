package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/security"
)

// env is an in-process registry behind real mutual TLS: a cluster CA, an
// admin and a node certificate, and a certificate from a different CA.
type env struct {
	t        *testing.T
	ca       *security.CA
	pool     *x509.CertPool
	admin    tls.Certificate
	node     tls.Certificate
	stranger tls.Certificate

	mu         sync.Mutex
	nodes      map[string]bool
	tags       []Tag
	failRecord bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ca, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return &env{
		t:        t,
		ca:       ca,
		pool:     pool,
		admin:    issueClient(t, ca, security.Identity{Role: security.RoleAdmin, Name: "admin"}),
		node:     issueClient(t, ca, security.Identity{Role: security.RoleNode, Name: "node-1"}),
		stranger: issueClient(t, other, security.Identity{Role: security.RoleAdmin, Name: "admin"}),
		nodes:    map[string]bool{"node-1": true},
	}
}

func issueClient(t *testing.T, ca *security.CA, id security.Identity) tls.Certificate {
	t.Helper()
	key, err := security.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.IssueClient(key.Public(), id, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

// serve starts a registry on dir. Several servers can share one dir.
func (e *env) serve(dir string, anonPull bool) *httptest.Server {
	e.t.Helper()
	h, err := New(e.t.Context(), Config{
		Dir:           dir,
		AnonymousPull: anonPull,
		NodeExists: func(_ context.Context, name string) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.nodes[name], nil
		},
		RecordTag: func(_ context.Context, tag Tag) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.failRecord {
				return fmt.Errorf("store unavailable")
			}
			e.tags = append(e.tags, tag)
			return nil
		},
		ListTags: func(context.Context) ([]Tag, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return append([]Tag(nil), e.tags...), nil
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	key, err := security.NewKey()
	if err != nil {
		e.t.Fatal(err)
	}
	leaf, err := e.ca.IssueServerFor(key.Public(), time.Now(), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    e.pool,
	}
	srv.StartTLS()
	e.t.Cleanup(srv.Close)
	return srv
}

// client returns an HTTP client that trusts the cluster CA and presents
// the given certificates (none for anonymous).
func (e *env) client(certs ...tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      e.pool,
		Certificates: certs,
	}}}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func do(t *testing.T, c *http.Client, method, u string, body []byte, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, out
}

const (
	ociManifest = "application/vnd.oci.image.manifest.v1+json"
	ociConfig   = "application/vnd.oci.image.config.v1+json"
	ociLayer    = "application/vnd.oci.image.layer.v1.tar"
)

// uploadBlob pushes data with the two-step upload protocol: start an upload,
// then complete it with the digest.
func uploadBlob(t *testing.T, c *http.Client, base, repo string, data []byte) string {
	t.Helper()
	d := digestOf(data)
	status, hdr, body := do(t, c, http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start blob upload: status %d: %s", status, body)
	}
	loc := hdr.Get("Location")
	if strings.HasPrefix(loc, "/") {
		loc = base + loc
	}
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	status, _, body = do(t, c, http.MethodPut, loc+sep+"digest="+url.QueryEscape(d), data,
		map[string]string{"Content-Type": "application/octet-stream"})
	if status != http.StatusCreated {
		t.Fatalf("complete blob upload: status %d: %s", status, body)
	}
	return d
}

// imageBody returns the manifest for a one-layer image.
func imageBody(t *testing.T, cfgD string, cfgLen int, layD string, layLen int) []byte {
	t.Helper()
	m, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifest,
		"config":        map[string]any{"mediaType": ociConfig, "digest": cfgD, "size": cfgLen},
		"layers":        []map[string]any{{"mediaType": ociLayer, "digest": layD, "size": layLen}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// imagePush uploads the blobs of a one-layer image and pushes its manifest
// under ref. It returns the manifest digest.
func imagePush(t *testing.T, c *http.Client, base, repo, ref string, cfg, layer []byte) string {
	t.Helper()
	cfgD := uploadBlob(t, c, base, repo, cfg)
	layD := uploadBlob(t, c, base, repo, layer)
	m := imageBody(t, cfgD, len(cfg), layD, len(layer))
	status, hdr, body := do(t, c, http.MethodPut, base+"/v2/"+repo+"/manifests/"+ref, m,
		map[string]string{"Content-Type": ociManifest})
	if status != http.StatusCreated {
		t.Fatalf("manifest put %s: status %d: %s", ref, status, body)
	}
	d := digestOf(m)
	if got := hdr.Get("Docker-Content-Digest"); got != d {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, d)
	}
	return d
}

// mustManifest fetches the manifest bytes for a reference.
func mustManifest(t *testing.T, c *http.Client, base, repo, ref string) []byte {
	t.Helper()
	status, _, body := do(t, c, http.MethodGet, base+"/v2/"+repo+"/manifests/"+ref, nil,
		map[string]string{"Accept": ociManifest})
	if status != http.StatusOK {
		t.Fatalf("get manifest %s: status %d", ref, status)
	}
	return body
}

func (e *env) tagRows() []Tag {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Tag(nil), e.tags...)
}

func TestPushPullAndTags(t *testing.T) {
	e := newEnv(t)
	srv := e.serve(t.TempDir(), false)
	admin := e.client(e.admin)
	cfg := []byte(`{"architecture":"amd64"}`)
	layer := []byte("layer bytes for the test image")

	md := imagePush(t, admin, srv.URL, "team/app", "v1", cfg, layer)
	if rows := e.tagRows(); len(rows) != 1 || rows[0].Repository != "team/app" || rows[0].Tag != "v1" || rows[0].Digest != md || rows[0].MediaType != ociManifest {
		t.Fatalf("tag rows after push = %+v", rows)
	}

	// A second tag on the same content adds a row with the same digest.
	if got := imagePush(t, admin, srv.URL, "team/app", "latest", cfg, layer); got != md {
		t.Fatalf("latest digest = %s, want %s", got, md)
	}

	// Pull by tag, then the layer blob.
	status, hdr, body := do(t, admin, http.MethodGet, srv.URL+"/v2/team/app/manifests/v1", nil,
		map[string]string{"Accept": ociManifest})
	if status != http.StatusOK || hdr.Get("Docker-Content-Digest") != md || digestOf(body) != md {
		t.Fatalf("pull manifest: status %d digest %q body digest %s", status, hdr.Get("Docker-Content-Digest"), digestOf(body))
	}
	status, _, got := do(t, admin, http.MethodGet, srv.URL+"/v2/team/app/blobs/"+digestOf(layer), nil, nil)
	if status != http.StatusOK || !bytes.Equal(got, layer) {
		t.Fatalf("pull blob: status %d, bytes equal %v", status, bytes.Equal(got, layer))
	}

	// Tag listing and catalog come from the registry itself.
	status, _, body = do(t, admin, http.MethodGet, srv.URL+"/v2/team/app/tags/list", nil, nil)
	var tl struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &tl); err != nil || status != http.StatusOK || len(tl.Tags) != 2 {
		t.Fatalf("tags/list: status %d body %s err %v", status, body, err)
	}
	status, _, body = do(t, admin, http.MethodGet, srv.URL+"/v2/_catalog", nil, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"team/app"`)) {
		t.Fatalf("catalog: status %d body %s", status, body)
	}

	// The admin tag endpoint returns the store rows.
	status, _, body = do(t, admin, http.MethodGet, srv.URL+TagsPath, nil, nil)
	var rows []Tag
	if err := json.Unmarshal(body, &rows); err != nil || status != http.StatusOK || len(rows) != 2 {
		t.Fatalf("%s: status %d body %s err %v", TagsPath, status, body, err)
	}

	// A push by digest is accepted and creates no tag row.
	before := len(e.tagRows())
	status, _, body = do(t, admin, http.MethodPut, srv.URL+"/v2/team/app/manifests/"+md, mustManifest(t, admin, srv.URL, "team/app", md),
		map[string]string{"Content-Type": ociManifest})
	if status != http.StatusCreated {
		t.Fatalf("digest push: status %d body %s", status, body)
	}
	if len(e.tagRows()) != before {
		t.Fatalf("digest push wrote a tag row")
	}
}

func TestTagStoreFailureFailsPush(t *testing.T) {
	e := newEnv(t)
	srv := e.serve(t.TempDir(), false)
	admin := e.client(e.admin)
	cfg, layer := []byte("{}"), []byte("x")

	e.mu.Lock()
	e.failRecord = true
	e.mu.Unlock()
	cfgD := uploadBlob(t, admin, srv.URL, "team/app", cfg)
	layD := uploadBlob(t, admin, srv.URL, "team/app", layer)
	m := imageBody(t, cfgD, len(cfg), layD, len(layer))
	put := func() int {
		status, _, _ := do(t, admin, http.MethodPut, srv.URL+"/v2/team/app/manifests/v1", m,
			map[string]string{"Content-Type": ociManifest})
		return status
	}
	if status := put(); status != http.StatusInternalServerError {
		t.Fatalf("push with failing store: status %d, want 500", status)
	}
	e.mu.Lock()
	e.failRecord = false
	e.mu.Unlock()
	// The client retries: the manifest is already there, the row is written now.
	if status := put(); status != http.StatusCreated {
		t.Fatalf("retry: status %d, want 201", status)
	}
	if rows := e.tagRows(); len(rows) != 1 || rows[0].Tag != "v1" {
		t.Fatalf("tag rows after retry = %+v", rows)
	}
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	noAnon := e.serve(dir, false)
	anon := e.serve(dir, true)
	imagePush(t, e.client(e.admin), noAnon.URL, "team/app", "v1", []byte("{}"), []byte("data"))

	manifest := noAnon.URL + "/v2/team/app/manifests/v1"
	pushBlob := noAnon.URL + "/v2/team/app/blobs/uploads/?digest=" + url.QueryEscape(digestOf([]byte("z")))
	anonPush := anon.URL + "/v2/team/app/blobs/uploads/?digest=" + url.QueryEscape(digestOf([]byte("z")))

	cases := []struct {
		name   string
		client *http.Client
		method string
		url    string
		want   int
	}{
		{"anonymous pull refused by default", e.client(), http.MethodGet, manifest, http.StatusUnauthorized},
		{"anonymous push refused", e.client(), http.MethodPost, pushBlob, http.StatusUnauthorized},
		{"anonymous catalog refused", e.client(), http.MethodGet, noAnon.URL + "/v2/_catalog", http.StatusUnauthorized},
		{"anonymous tags endpoint refused", e.client(), http.MethodGet, noAnon.URL + TagsPath, http.StatusUnauthorized},
		{"node pull allowed", e.client(e.node), http.MethodGet, manifest, http.StatusOK},
		{"node push refused", e.client(e.node), http.MethodPost, pushBlob, http.StatusUnauthorized},
		{"node catalog refused", e.client(e.node), http.MethodGet, noAnon.URL + "/v2/_catalog", http.StatusUnauthorized},
		{"node tags endpoint refused", e.client(e.node), http.MethodGet, noAnon.URL + TagsPath, http.StatusUnauthorized},
		{"admin catalog allowed", e.client(e.admin), http.MethodGet, noAnon.URL + "/v2/_catalog", http.StatusOK},
		{"anonymous pull allowed when enabled", e.client(), http.MethodGet, anon.URL + "/v2/team/app/manifests/v1", http.StatusOK},
		{"anonymous push refused when pull is enabled", e.client(), http.MethodPost, anonPush, http.StatusUnauthorized},
		{"anonymous catalog refused when pull is enabled", e.client(), http.MethodGet, anon.URL + "/v2/_catalog", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := do(t, tc.client, tc.method, tc.url, nil, map[string]string{"Accept": ociManifest})
			if status != tc.want {
				t.Fatalf("status %d, want %d", status, tc.want)
			}
		})
	}

	t.Run("certificate from another CA is refused at the handshake", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, manifest, nil)
		if resp, err := e.client(e.stranger).Do(req); err == nil {
			resp.Body.Close()
			t.Fatal("handshake with a foreign client certificate succeeded")
		}
	})

	t.Run("deleted node loses pull even when anonymous pull is on", func(t *testing.T) {
		e.mu.Lock()
		e.nodes["node-1"] = false
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.nodes["node-1"] = true
			e.mu.Unlock()
		}()
		status, _, _ := do(t, e.client(e.node), http.MethodGet, anon.URL+"/v2/team/app/manifests/v1", nil, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", status)
		}
	})
}
