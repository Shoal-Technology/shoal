package objstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testBucket = "shoal-backups"
	testKey    = "AKIATESTKEYID0001"
	testSecret = "s3cr3t-value-DO-NOT-LEAK-7f3a9c"
)

// fakeS3 is an in-process bucket that speaks the path-style PUT, GET, HEAD and
// DELETE calls minio-go makes for single-part objects.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	denyPuts bool
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := "/" + testBucket + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)
	switch r.Method {
	case http.MethodPut:
		if f.denyPuts {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		var body []byte
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			var err error
			if body, err = decodeChunked(bufio.NewReader(r.Body)); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			body, _ = io.ReadAll(r.Body)
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(body)
	case http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// decodeChunked reads an aws-chunked body ("<hex size>;chunk-signature=...\r\n
// <data>\r\n", ending with a zero-size chunk) and returns the payload. The
// signatures are not checked: this fake tests the upload path, not SigV4.
func decodeChunked(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("chunk header: %w", err)
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("chunk size %q: %w", sizeHex, err)
		}
		if n == 0 {
			return out, nil
		}
		buf := make([]byte, n+2) // data plus its CRLF
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("chunk data: %w", err)
		}
		out = append(out, buf[:n]...)
	}
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func testClient(t *testing.T, srv *httptest.Server, prefix string, log *slog.Logger) *Client {
	t.Helper()
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	c, err := New(Config{
		Endpoint: endpoint,
		Bucket:   testBucket,
		Prefix:   prefix,
		Insecure: true,
		Log:      log,
	}, Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeDump(t *testing.T, content string) (string, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	p := filepath.Join(t.TempDir(), "bk1.dump")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, hex.EncodeToString(sum[:])
}

func TestKeyLayout(t *testing.T) {
	c := testClient(t, httptest.NewServer(http.NotFoundHandler()), "backups/", nil)
	k, err := c.Key("blog-db", "bk1")
	if err != nil || k != "backups/blog-db/bk1.dump" {
		t.Fatalf("Key = %q, %v; want backups/blog-db/bk1.dump", k, err)
	}
	bare := testClient(t, httptest.NewServer(http.NotFoundHandler()), "", nil)
	if k, _ := bare.Key("blog-db", "bk1"); k != "blog-db/bk1.dump" {
		t.Fatalf("no-prefix Key = %q", k)
	}
	for _, bad := range [][2]string{{"..", "bk1"}, {"blog-db", "a/b"}, {"", "bk1"}} {
		if _, err := c.Key(bad[0], bad[1]); err == nil {
			t.Errorf("Key(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestUploadPathAndContent(t *testing.T) {
	fake, srv := newFakeS3(t)
	c := testClient(t, srv, "backups", nil)
	file, _ := writeDump(t, "PGDMP-test-bytes")
	key, _ := c.Key("blog-db", "bk1")
	if err := c.Upload(context.Background(), file, key); err != nil {
		t.Fatal(err)
	}
	got, ok := fake.get("backups/blog-db/bk1.dump")
	if !ok || string(got) != "PGDMP-test-bytes" {
		t.Fatalf("object at backups/blog-db/bk1.dump = %q, ok=%v", got, ok)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("local copy removed by a successful upload: %v", err)
	}
}

func TestFailedUploadKeepsLocalCopy(t *testing.T) {
	fake, srv := newFakeS3(t)
	fake.denyPuts = true
	c := testClient(t, srv, "", nil)
	file, _ := writeDump(t, "local-only")
	err := c.Upload(context.Background(), file, "blog-db/bk2.dump")
	if err == nil {
		t.Fatal("upload to a denying bucket succeeded")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "local-only" {
		t.Fatalf("local copy after failed upload: %q, %v", b, err)
	}
}

func TestDownloadVerifiesSHA(t *testing.T) {
	fake, srv := newFakeS3(t)
	c := testClient(t, srv, "", nil)
	file, sum := writeDump(t, "remote-dump")
	if err := c.Upload(context.Background(), file, "blog-db/bk3.dump"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil { // remote-only: the local dump is gone
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restore.dump")
	if err := c.Download(context.Background(), "blog-db/bk3.dump", dest, sum); err != nil {
		t.Fatalf("download with the right sha: %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "remote-dump" {
		t.Fatalf("downloaded %q", b)
	}

	bad := filepath.Join(t.TempDir(), "bad.dump")
	err := c.Download(context.Background(), "blog-db/bk3.dump", bad, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("download with a wrong sha: err = %v", err)
	}
	if _, statErr := os.Stat(bad); !os.IsNotExist(statErr) {
		t.Fatalf("a file that failed the sha check was kept: %v", statErr)
	}
	_ = fake
}

func TestExistsAndDeleteRemoveRemoteObject(t *testing.T) {
	fake, srv := newFakeS3(t)
	c := testClient(t, srv, "p", nil)
	file, _ := writeDump(t, "x")
	ctx := context.Background()
	if err := c.Upload(ctx, file, "p/blog-db/bk4.dump"); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Exists(ctx, "p/blog-db/bk4.dump"); err != nil || !ok {
		t.Fatalf("Exists after upload = %v, %v", ok, err)
	}
	if err := c.Delete(ctx, "p/blog-db/bk4.dump"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.get("p/blog-db/bk4.dump"); ok {
		t.Fatal("object still in the bucket after Delete")
	}
	if ok, err := c.Exists(ctx, "p/blog-db/bk4.dump"); err != nil || ok {
		t.Fatalf("Exists after delete = %v, %v", ok, err)
	}
	if err := c.Delete(ctx, "p/blog-db/bk4.dump"); err != nil {
		t.Fatalf("second Delete of a missing key: %v", err)
	}
}

func TestCredentialsNeverInLogsOrErrors(t *testing.T) {
	fake, srv := newFakeS3(t)
	fake.denyPuts = true
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := testClient(t, srv, "", log)
	file, _ := writeDump(t, "x")
	upErr := c.Upload(context.Background(), file, "blog-db/bk5.dump")
	if upErr == nil {
		t.Fatal("expected a failed upload")
	}
	if _, err := New(Config{Endpoint: "", Bucket: "b"}, Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret}); err == nil {
		t.Fatal("New accepted an empty endpoint")
	}
	creds := Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret}
	printed := fmt.Sprintf("%v %+v %#v %s", creds, creds, creds, creds)

	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for name, text := range map[string]string{"log": logs, "error": upErr.Error(), "printed": printed} {
		if strings.Contains(text, testSecret) {
			t.Errorf("secret found in %s: %s", name, text)
		}
	}
	if !strings.Contains(logs, "backup upload failed") {
		t.Errorf("the log did not record the failure: %q", logs)
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
