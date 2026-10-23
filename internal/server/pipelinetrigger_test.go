package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/memory"
)

func TestSameRepository(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://example.com/acme/web.git", "https://example.com/acme/web", true},
		{"https://example.com/acme/web/", "https://EXAMPLE.com/acme/web.git", true},
		{"https://example.com/acme/web", "https://example.com/acme/api", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := sameRepository(c.a, c.b); got != c.want {
			t.Errorf("sameRepository(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// gitRepo returns a repository with two commits of shoal-ci.yml and the
// first commit's sha.
func gitRepo(t *testing.T) (dir, firstSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("git unavailable: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "shoal-ci.yml"), []byte("test:\n  image: x\n  script: [a]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "shoal-ci.yml")
	run("commit", "-q", "-m", "one")
	firstSHA = run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "shoal-ci.yml"), []byte("changed: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-q", "-am", "two")
	return dir, firstSHA
}

// TestReadPipelineFileReadsTheCommit: the file comes from the named commit,
// not from the branch's head.
func TestReadPipelineFileReadsTheCommit(t *testing.T) {
	repo, first := gitRepo(t)
	data, err := readPipelineFile(t.Context(), repo, &v1alpha1.BuildCredentials{}, first, "")
	if err != nil {
		t.Fatalf("read at %s: %v", first, err)
	}
	if !strings.Contains(string(data), "image: x") {
		t.Errorf("file at the first commit = %q", data)
	}
}

func TestReadPipelineFileRefusesAPathOutsideTheRepository(t *testing.T) {
	_, err := readPipelineFile(t.Context(), "https://example.com/acme/web.git", nil, "0123", "../secrets.yml")
	if err == nil || !strings.Contains(err.Error(), "leaves the repository") {
		t.Errorf("error = %v, want a refusal of the path", err)
	}
}

// TestPipelineRunForCommitFindsThePushRun: a push run of the commit is found,
// and a run of another trigger or commit is not.
func TestPipelineRunForCommitFindsThePushRun(t *testing.T) {
	ctx := t.Context()
	st := memory.New(time.Now)
	put := func(name string, spec *v1alpha1.PipelineRunSpec) {
		t.Helper()
		b, err := objects.Encode(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Create(ctx, store.Record{Kind: v1alpha1.KindPipelineRun, Name: name, Spec: b}); err != nil {
			t.Fatal(err)
		}
	}
	put("web-ci-a", &v1alpha1.PipelineRunSpec{Pipeline: "web-ci", Commit: "aaa", Trigger: "manual"})
	put("web-ci-b", &v1alpha1.PipelineRunSpec{Pipeline: "web-ci", Commit: "bbb", Trigger: "push"})
	s := &Server{store: st}

	if name, ok, err := s.pipelineRunForCommit(ctx, "web-ci", "bbb"); err != nil || !ok || name != "web-ci-b" {
		t.Errorf("push run of bbb = %q, %v, %v; want web-ci-b", name, ok, err)
	}
	if _, ok, _ := s.pipelineRunForCommit(ctx, "web-ci", "aaa"); ok {
		t.Error("a manual run counted as the push run of its commit")
	}
	if _, ok, _ := s.pipelineRunForCommit(ctx, "other", "bbb"); ok {
		t.Error("a run of another pipeline counted")
	}
}
