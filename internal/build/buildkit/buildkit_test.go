package buildkit

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

func TestWithinRefusesEscapes(t *testing.T) {
	base := "/work/src"
	for _, rel := range []string{"", ".", "app", "app/cmd"} {
		if _, err := within(base, rel); err != nil {
			t.Errorf("within(%q) refused: %v", rel, err)
		}
	}
	for _, rel := range []string{"..", "../etc", "app/../../x"} {
		if p, err := within(base, rel); err == nil {
			t.Errorf("within(%q) = %q, want an error", rel, p)
		}
	}
}

func TestShellQuoteKeepsValuesLiteral(t *testing.T) {
	got := shellQuote("a'b $(rm -rf /)")
	if want := `'a'\''b $(rm -rf /)'`; got != want {
		t.Fatalf("shellQuote = %s, want %s", got, want)
	}
}

func TestRedactURLDropsUserInfo(t *testing.T) {
	if got := redactURL("https://bob:tok@github.com/acme/web.git"); got != "https://github.com/acme/web.git" {
		t.Fatalf("redactURL = %q", got)
	}
	if got := redactURL("git@github.com:acme/web.git"); got != "git@github.com:acme/web.git" {
		t.Fatalf("scp-style URL changed: %q", got)
	}
}

func TestReadDigest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(p, []byte(`{"containerimage.digest":"sha256:abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := readDigest(p); err != nil || d != "sha256:abc" {
		t.Fatalf("readDigest = %q, %v", d, err)
	}
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDigest(p); err == nil {
		t.Fatal("a metadata file without a digest was accepted")
	}
}

func TestDaemonFilesAndScript(t *testing.T) {
	dir := t.TempDir()
	job := Job{
		Build:         "web-abc12",
		Source:        &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Url: "https://h/x", Ref: "main", ContextDir: "app", Dockerfile: "docker/Dockerfile"}, Output: &v1alpha1.BuildOutput{Repository: "web", Tag: "{short-sha}"}, BuildArgs: map[string]string{"B": "2", "A": "it's"}},
		Registry:      "10.0.0.5:5000",
		Dir:           dir,
		ClientCertPEM: []byte("CERT"),
		ClientKeyPEM:  []byte("KEY"),
		CAPEM:         []byte("CA"),
	}
	if err := writeDaemonFiles(job); err != nil {
		t.Fatal(err)
	}
	toml, err := os.ReadFile(filepath.Join(dir, "buildkitd.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`[registry."10.0.0.5:5000"]`, `ca = ["/shoal-build/pki/ca.crt"]`, `key = "/shoal-build/pki/builder.key"`, `cert = "/shoal-build/pki/builder.crt"`} {
		if !strings.Contains(string(toml), want) {
			t.Errorf("buildkitd.toml lacks %s:\n%s", want, toml)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); err != nil {
		t.Fatalf("state directory: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "src", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeScript(job, filepath.Join(dir, "src", "app"), filepath.Join(dir, "src", "app", "docker", "Dockerfile"), "10.0.0.5:5000/web:abc1234"); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	for _, want := range []string{
		"'context=/shoal-build/src/app'",
		"'dockerfile=/shoal-build/src/app/docker'",
		"'filename=Dockerfile'",
		"'build-arg:A=it'\\''s'",
		"'build-arg:B=2'",
		"'type=image,name=10.0.0.5:5000/web:abc1234,push=true'",
		"'--metadata-file' '/shoal-build/metadata.json'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("run.sh lacks %s:\n%s", want, s)
		}
	}
	if strings.Index(s, "'build-arg:A") > strings.Index(s, "'build-arg:B") {
		t.Error("build args are not in key order")
	}
}

func TestSSHCredentialsNeedKnownHosts(t *testing.T) {
	job := Job{Dir: t.TempDir(), Credentials: &v1alpha1.BuildCredentials{
		Type:          resource.SecretTypeSSHAuth,
		SshPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\n",
	}}
	if _, err := gitEnv(job); err == nil {
		t.Fatal("an ssh key without known_hosts was accepted")
	}
}

// gitEnv must keep credentials out of argv and out of the URL.
func TestBasicAuthIsInEnvironmentOnly(t *testing.T) {
	job := Job{Dir: t.TempDir(), Credentials: &v1alpha1.BuildCredentials{
		Type: resource.SecretTypeBasicAuth, Username: "bob", Password: "s3cret-pass",
	}}
	env, err := gitEnv(job)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SHOAL_GIT_PASSWORD=s3cret-pass") {
		t.Fatal("password is not passed in the environment")
	}
	script, err := os.ReadFile(filepath.Join(job.Dir, "git", "askpass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(script, []byte("s3cret-pass")) {
		t.Fatal("the password was written into the askpass script")
	}
}

// TestCloneLocalRepository runs the real git clone against a local repository.
func TestCloneLocalRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "first")
	want := run("rev-parse", "HEAD")

	job := Job{Dir: t.TempDir(), Source: &v1alpha1.BuildConfigSpec{Git: &v1alpha1.GitSource{Url: "file://" + filepath.ToSlash(repo), Ref: "main"}}}
	var log bytes.Buffer
	got, err := clone(t.Context(), job, filepath.Join(job.Dir, "src"), &log)
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, log.String())
	}
	if got != want {
		t.Fatalf("commit %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(job.Dir, "src", "Dockerfile")); err != nil {
		t.Fatalf("checkout: %v", err)
	}
}

// The registry must be reachable and the node's other addresses must not:
// the accept for the registry comes before any reject.
func TestIsolationRulesOrder(t *testing.T) {
	reg := net.ParseIP("10.1.0.5").To4()
	rules := isolationRules(reg, "5443", []net.IP{net.ParseIP("10.1.0.5").To4(), net.ParseIP("10.250.1.1").To4()})
	index := func(want string) int {
		for i, r := range rules {
			if strings.Join(r, " ") == want {
				return i
			}
		}
		return -1
	}
	regAccept := index("iptables -w -A OUTPUT -d 10.1.0.5 -p tcp --dport 5443 -j ACCEPT")
	nodeReject := index("iptables -w -A OUTPUT -d 10.1.0.5 -j REJECT")
	if regAccept < 0 || nodeReject < 0 || regAccept > nodeReject {
		t.Fatalf("registry accept at %d, node reject at %d: the registry must come first", regAccept, nodeReject)
	}
	for _, want := range []string{
		"iptables -w -A OUTPUT -d 10.0.0.0/8 -j REJECT",
		"iptables -w -A OUTPUT -d 169.254.0.0/16 -j REJECT",
		"iptables -w -A OUTPUT -d 10.250.1.1 -j REJECT",
		"ip6tables -w -A OUTPUT -j REJECT",
	} {
		if index(want) < 0 {
			t.Errorf("no rule %q", want)
		}
	}
	if last := strings.Join(rules[len(rules)-1], " "); last != "ip6tables -w -A OUTPUT -j REJECT" {
		t.Errorf("last rule %q", last)
	}
}

// The build daemon is the only container that clears no_new_privs. It has no
// devices, shares only its own network namespace (--net=host means the
// container's), and waits for the firewall's gate before it starts (ADR 0021).
func TestDaemonSpecIsTheOnlyPrivilegeException(t *testing.T) {
	spec := daemonSpec(Job{Build: "web-abc12", Dir: "/var/lib/shoal/builds/web-abc12"}, "shoal-build-web-abc12")
	if !spec.AllowPrivilegeEscalation {
		t.Fatal("the daemon does not clear no_new_privs; RootlessKit needs it")
	}
	if spec.Labels[LabelBuild] != "web-abc12" {
		t.Fatalf("build label %q", spec.Labels[LabelBuild])
	}
	script := strings.Join(spec.Command, " ")
	if !strings.Contains(script, "--net=host") || strings.Contains(script, "gvisor") || strings.Contains(script, "slirp") {
		t.Fatalf("daemon command %q: the network must be the container's own namespace", script)
	}
	if !strings.Contains(script, mount+"/"+gateName) {
		t.Fatalf("daemon command %q does not wait for the firewall's gate", script)
	}
}
