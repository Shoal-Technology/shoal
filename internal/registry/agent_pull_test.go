package registry

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// TestAgentPullsOverMutualTLS is the agent side of ADR 0020, pass B. A node
// reaches the registry through runtime.Registry, the same TLS settings the
// containerd resolver uses: the cluster CA and the node's client certificate.
// It covers the shoal.local alias, a node whose Node object was deleted, a
// client certificate from another CA, and an agent that does not trust the
// cluster CA.
func TestAgentPullsOverMutualTLS(t *testing.T) {
	e := newEnv(t)
	srv := e.serve(t.TempDir(), false)
	addr := strings.TrimPrefix(srv.URL, "https://")
	digest := imagePush(t, e.client(e.admin), srv.URL, "team/app", "v1", []byte(`{"architecture":"amd64"}`), []byte("layer"))

	node := &runtime.Registry{Address: addr, CA: e.ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return &e.node, nil }}
	agent := node.HTTPClient()
	if got := node.MapImage("shoal.local/team/app:v1"); got != addr+"/team/app:v1" {
		t.Fatalf("alias maps to %q", got)
	}

	// The node pulls the manifest by tag, and the digest matches the push.
	status, hdr, _ := do(t, agent, http.MethodGet, srv.URL+"/v2/team/app/manifests/v1", nil,
		map[string]string{"Accept": ociManifest})
	if status != http.StatusOK {
		t.Fatalf("node pull by tag: status %d", status)
	}
	if got := hdr.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("node pull digest = %q, want %q", got, digest)
	}

	// A node pull cannot push: the policy is unchanged for agents.
	if status, _, _ := do(t, agent, http.MethodPut, srv.URL+"/v2/team/app/manifests/v2", []byte("{}"),
		map[string]string{"Content-Type": ociManifest}); status == http.StatusCreated {
		t.Fatal("a node certificate pushed a manifest")
	}

	// Deleting the Node object revokes its pulls (ADR 0007).
	e.mu.Lock()
	e.nodes["node-1"] = false
	e.mu.Unlock()
	if status, _, _ := do(t, agent, http.MethodGet, srv.URL+"/v2/team/app/manifests/v1", nil,
		map[string]string{"Accept": ociManifest}); status != http.StatusUnauthorized {
		t.Fatalf("deleted node pull: status %d, want 401", status)
	}

	// A client certificate from another CA never gets through the handshake.
	stranger := &runtime.Registry{Address: addr, CA: e.ca.Cert, ClientCertificate: func() (*tls.Certificate, error) { return &e.stranger, nil }}
	if resp, err := stranger.HTTPClient().Get(srv.URL + "/v2/"); err == nil {
		resp.Body.Close()
		t.Fatalf("stranger certificate: status %d, want a failed handshake", resp.StatusCode)
	}

	// An agent that trusts another CA refuses the registry's certificate.
	other, _, err := security.LoadOrCreateCA(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	untrusting := &runtime.Registry{Address: addr, CA: other.Cert, ClientCertificate: func() (*tls.Certificate, error) { return &e.node, nil }}
	if resp, err := untrusting.HTTPClient().Get(srv.URL + "/v2/"); err == nil {
		resp.Body.Close()
		t.Fatalf("untrusting agent: status %d, want a failed verification", resp.StatusCode)
	}
}
