// Package buildkit runs one Build on a builder node (ADR 0021, pass B). It
// clones the repository on the host with git, runs a rootless BuildKit daemon
// in a container through the node's runtime, builds the Dockerfile, and pushes
// the image to the embedded registry with the Build's push certificate.
//
// Everything a run writes lives in one job directory, which the caller removes
// when the run ends. The build log is <dir>/build.log. The credentials are
// written there for the clone only and removed before the build starts.
package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
)

// Image is the BuildKit daemon: moby/buildkit, rootless, pinned by digest
// (ADR 0021, pass B). The digest was read from the registry for v0.32.2.
// Update the tag and the digest together.
const Image = "docker.io/moby/buildkit:v0.32.2-rootless@sha256:504731e577c20559c00f968f33219f30115e70be29ab96728d1d06e963fc494b"

// DefaultTimeout bounds one build, the clone and the push. The builder's
// push certificate is valid for longer than this (security.BuildCertificateValidity).
const DefaultTimeout = 90 * time.Minute

const (
	// mount is where the job directory appears inside the BuildKit container.
	mount = "/shoal-build"
	// socket is the daemon's API socket, inside the job directory.
	socket = mount + "/state/buildkitd.sock"
	// uid of the rootless BuildKit user in the image. The job directory is
	// owned by it, so the daemon can write its state and read the context.
	buildkitUID = 1000

	readyTimeout = 2 * time.Minute
	stopTimeout  = 10 * time.Second
	// logName is the build log inside the job directory.
	logName = "build.log"
	// metadataName is the file BuildKit writes the pushed image's digest to.
	metadataName = "metadata.json"
)

// Job is one build for a builder node.
type Job struct {
	Build  string
	Source *v1alpha1.BuildConfigSpec
	// Credentials are the git credentials from the Build's Secret. Empty for a
	// public repository.
	Credentials *v1alpha1.BuildCredentials
	// Registry is the host:port of the embedded registry to push to.
	Registry string
	// Dir is the job directory. It must exist and be owned by root.
	Dir string
	// ClientCertPEM and ClientKeyPEM are the Build's push certificate and its
	// key. CAPEM is the cluster CA, which signs the registry's certificate.
	ClientCertPEM, ClientKeyPEM, CAPEM []byte
	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Result is a successful build.
type Result struct {
	Commit string
	Image  string // repository:tag pushed to the registry
	Digest string // manifest digest of the pushed image
}

// LabelBuild marks a daemon container with its Build. The node's reconciler
// leaves such containers alone: they are not replicas (ADR 0021, pass B).
const LabelBuild = "shoal.dev/build"

// LogFile is the build log of a job directory.
func LogFile(dir string) string { return filepath.Join(dir, logName) }

// Run builds and pushes one Build. A failed build returns an error that says
// why. The container is removed before Run returns, whatever the outcome.
func Run(ctx context.Context, rt runtime.Runtime, job Job) (Result, error) {
	if err := validate(job); err != nil {
		return Result{}, err
	}
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logf, err := os.OpenFile(LogFile(job.Dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("open build log: %w", err)
	}
	defer logf.Close()

	src := filepath.Join(job.Dir, "src")
	commit, err := clone(ctx, job, src, logf)
	if err != nil {
		return Result{}, err
	}
	tag, err := resource.ExpandTag(job.Source.GetOutput().GetTag(), commit, job.Source.GetGit().GetRef())
	if err != nil {
		return Result{}, err
	}
	repo := job.Source.GetOutput().GetRepository()
	image := job.Registry + "/" + repo + ":" + tag

	ctxDir, dockerfile, err := contextPaths(src, job.Source.GetGit())
	if err != nil {
		return Result{}, err
	}
	if err := writeDaemonFiles(job); err != nil {
		return Result{}, err
	}
	if err := writeScript(job, ctxDir, dockerfile, image); err != nil {
		return Result{}, err
	}
	// The clone's credentials were only needed for the clone.
	if err := os.RemoveAll(filepath.Join(job.Dir, "git")); err != nil {
		return Result{}, fmt.Errorf("remove git credentials: %w", err)
	}
	// The daemon runs as buildkitUID and must read and write the job tree.
	if err := chownTree(job.Dir, buildkitUID); err != nil {
		return Result{}, err
	}

	id := containerID(job.Build)
	_ = rt.Remove(context.Background(), id) // a leftover from an earlier run
	defer cleanup(rt, id)
	if err := rt.EnsureImage(ctx, Image, nil); err != nil {
		return Result{}, fmt.Errorf("pull the BuildKit image: %w", err)
	}
	if err := rt.Create(ctx, daemonSpec(job, id)); err != nil {
		return Result{}, fmt.Errorf("create the BuildKit container: %w", err)
	}
	if err := rt.Start(ctx, id); err != nil {
		return Result{}, fmt.Errorf("start the BuildKit container: %w", err)
	}
	// Fail closed: the daemon's network is firewalled before any build runs.
	if err := isolate(ctx, rt, id, job.Registry); err != nil {
		return Result{}, fmt.Errorf("isolate the build network: %w", err)
	}
	if err := os.WriteFile(filepath.Join(job.Dir, gateName), nil, 0o644); err != nil {
		return Result{}, fmt.Errorf("open the build gate: %w", err)
	}
	if err := waitReady(ctx, rt, id); err != nil {
		// The daemon's own output says why it did not start. Keep it in the
		// build log before the container is removed.
		dumpDaemonLogs(rt, id, logf)
		return Result{}, err
	}
	res, err := rt.Exec(ctx, id, []string{"sh", mount + "/run.sh"}, timeout)
	if err != nil {
		return Result{}, fmt.Errorf("run the build: %w", err)
	}
	if res.ExitCode != 0 {
		return Result{}, fmt.Errorf("the build failed (exit %d); see the build log", res.ExitCode)
	}
	digest, err := readDigest(filepath.Join(job.Dir, metadataName))
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: commit, Image: repo + ":" + tag, Digest: digest}, nil
}

// CloneRepo fetches job.Source's repository into src, checks out its ref and
// returns the commit. It is the clone a Build does, with the same credential
// handling (ADR 0021, section 2). Pipeline jobs use it (ADR 0022, pass B).
func CloneRepo(ctx context.Context, job Job, src string, log io.Writer) (string, error) {
	return clone(ctx, job, src, log)
}

// Isolate firewalls the network namespace of a container, with the rules a
// build's daemon gets (ADR 0021). An empty registry adds no registry exception.
// Pipeline jobs use it (ADR 0022, section 5).
func Isolate(ctx context.Context, rt runtime.Runtime, id, registry string) error {
	return isolate(ctx, rt, id, registry)
}

func validate(job Job) error {
	switch {
	case job.Build == "":
		return errors.New("buildkit: build name is required")
	case job.Source.GetGit().GetUrl() == "":
		return errors.New("buildkit: the build has no git URL")
	case job.Source.GetOutput().GetRepository() == "":
		return errors.New("buildkit: the build has no output repository")
	case job.Registry == "":
		return errors.New("buildkit: no registry address")
	case job.Dir == "":
		return errors.New("buildkit: no job directory")
	case len(job.ClientCertPEM) == 0 || len(job.ClientKeyPEM) == 0 || len(job.CAPEM) == 0:
		return errors.New("buildkit: the push certificate and the CA are required")
	case strings.Contains(Image, "TODO"):
		return errors.New("buildkit: the BuildKit image is not pinned")
	}
	return nil
}

// clone fetches the repository into src and checks out the ref. It returns
// the commit the ref resolved to.
func clone(ctx context.Context, job Job, src string, log io.Writer) (string, error) {
	env, err := gitEnv(job)
	if err != nil {
		return "", err
	}
	url := job.Source.GetGit().GetUrl()
	if err := runHost(ctx, env, log, "git", "clone", "--quiet", url, src); err != nil {
		return "", fmt.Errorf("clone %s: %w", redactURL(url), err)
	}
	ref := job.Source.GetGit().GetRef()
	if ref != "" && ref != "HEAD" {
		// A branch from the remote first, then a tag or a commit.
		if err := runHost(ctx, env, log, "git", "-C", src, "checkout", "--quiet", "--detach", "origin/"+ref); err != nil {
			if err := runHost(ctx, env, log, "git", "-C", src, "checkout", "--quiet", "--detach", ref); err != nil {
				return "", fmt.Errorf("ref %q: not found in the repository", ref)
			}
		}
	}
	out, err := hostOutput(ctx, env, "git", "-C", src, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// gitEnv returns the environment for git. Credentials go in the environment,
// never in argv or the URL. Git reads them through a helper script that
// prints them, and an ssh key is passed as a file.
func gitEnv(job Job) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "HOME=" + job.Dir, "PATH=" + os.Getenv("PATH")}
	creds := job.Credentials
	if creds.GetType() == "" {
		return env, nil
	}
	dir := filepath.Join(job.Dir, "git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	switch creds.GetType() {
	case resource.SecretTypeBasicAuth:
		askpass := filepath.Join(dir, "askpass.sh")
		const script = "#!/bin/sh\ncase \"$1\" in\n  Username*) printf '%s' \"$SHOAL_GIT_USERNAME\" ;;\n  *) printf '%s' \"$SHOAL_GIT_PASSWORD\" ;;\nesac\n"
		if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
			return nil, err
		}
		env = append(env, "GIT_ASKPASS="+askpass,
			"SHOAL_GIT_USERNAME="+creds.GetUsername(), "SHOAL_GIT_PASSWORD="+creds.GetPassword(),
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=")
	case resource.SecretTypeSSHAuth:
		if creds.GetKnownHosts() == "" {
			// A build never trusts an unknown host (ADR 0021, section 2).
			return nil, errors.New("the ssh-auth Secret has no known_hosts: the host key cannot be checked")
		}
		key := filepath.Join(dir, "id")
		kh := filepath.Join(dir, "known_hosts")
		if err := os.WriteFile(key, []byte(creds.GetSshPrivateKey()), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(kh, []byte(creds.GetKnownHosts()), 0o600); err != nil {
			return nil, err
		}
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+key+" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="+kh)
	default:
		return nil, fmt.Errorf("unknown credential type %q", creds.GetType())
	}
	return env, nil
}

// contextPaths returns the build context and the Dockerfile inside src. Both
// must stay inside the repository.
func contextPaths(src string, g *v1alpha1.GitSource) (ctxDir, dockerfile string, err error) {
	ctxDir, err = within(src, g.GetContextDir())
	if err != nil {
		return "", "", fmt.Errorf("context directory: %w", err)
	}
	dockerfile, err = within(ctxDir, g.GetDockerfile())
	if err != nil {
		return "", "", fmt.Errorf("dockerfile: %w", err)
	}
	if _, err := os.Stat(dockerfile); err != nil {
		return "", "", fmt.Errorf("dockerfile %q not found in the repository", g.GetDockerfile())
	}
	return ctxDir, dockerfile, nil
}

// within joins rel onto base and refuses a path that leaves base.
func within(base, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q is absolute", rel)
	}
	p := filepath.Join(base, rel)
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q leaves the repository", rel)
	}
	return p, nil
}

// writeDaemonFiles writes the registry configuration and the push certificate
// that the daemon reads.
func writeDaemonFiles(job Job) error {
	pki := filepath.Join(job.Dir, "pki")
	if err := os.MkdirAll(pki, 0o700); err != nil {
		return err
	}
	// The daemon's socket, its state and RootlessKit's runtime directory.
	for _, d := range []string{"state", "state/xdg"} {
		if err := os.MkdirAll(filepath.Join(job.Dir, d), 0o700); err != nil {
			return err
		}
	}
	files := map[string][]byte{
		"ca.crt":      job.CAPEM,
		"builder.crt": job.ClientCertPEM,
		"builder.key": job.ClientKeyPEM,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(pki, name), data, 0o600); err != nil {
			return err
		}
	}
	toml := fmt.Sprintf(`[registry.%q]
  ca = [%q]
  [[registry.%q.keypair]]
    key = %q
    cert = %q
`, job.Registry, mount+"/pki/ca.crt", job.Registry, mount+"/pki/builder.key", mount+"/pki/builder.crt")
	return os.WriteFile(filepath.Join(job.Dir, "buildkitd.toml"), []byte(toml), 0o600)
}

// writeScript writes run.sh, which the container runs. Every value is quoted,
// so the repository cannot change the command.
func writeScript(job Job, ctxDir, dockerfile, image string) error {
	// A path under the job's src directory, as the container sees it.
	rel := func(p string) string {
		return mount + "/src" + filepath.ToSlash(strings.TrimPrefix(p, filepath.Join(job.Dir, "src")))
	}
	args := []string{
		"buildctl", "--addr", "unix://" + socket, "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + rel(ctxDir),
		"--local", "dockerfile=" + rel(filepath.Dir(dockerfile)),
		"--opt", "filename=" + filepath.Base(dockerfile),
	}
	keys := make([]string, 0, len(job.Source.GetBuildArgs()))
	for k := range job.Source.GetBuildArgs() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+job.Source.GetBuildArgs()[k])
	}
	args = append(args,
		"--output", "type=image,name="+image+",push=true",
		"--metadata-file", mount+"/"+metadataName,
	)
	var b bytes.Buffer
	b.WriteString("#!/bin/sh\nset -e\nexec ")
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(a))
	}
	b.WriteString(" >>" + mount + "/" + logName + " 2>&1\n")
	return os.WriteFile(filepath.Join(job.Dir, "run.sh"), b.Bytes(), 0o700)
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitReady waits until the daemon answers. When it does not, the error says
// what the last probe reported and the state of the container.
func waitReady(ctx context.Context, rt runtime.Runtime, id string) error {
	deadline := time.Now().Add(readyTimeout)
	var last string
	for {
		res, err := rt.Exec(ctx, id, []string{"buildctl", "--addr", "unix://" + socket, "debug", "workers"}, 10*time.Second)
		switch {
		case err == nil && res.ExitCode == 0:
			return nil
		case err != nil:
			last = err.Error()
		default:
			last = fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if time.Now().After(deadline) {
			detail := last
			if st, err := rt.Status(ctx, id); err == nil {
				detail += fmt.Sprintf("; the daemon is %s (exit %d)", st.State, st.ExitCode)
			}
			return fmt.Errorf("the BuildKit daemon did not start: %s", detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// dumpDaemonLogs appends the daemon container's output to the build log, marked
// "daemon:". It is best effort: the build has already failed.
func dumpDaemonLogs(rt runtime.Runtime, id string, log io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = rt.Logs(ctx, id, runtime.LogOptions{Tail: -1}, func(e runtime.LogEntry) error {
		_, err := fmt.Fprintf(log, "daemon: %s\n", e.Line)
		return err
	})
}

// readDigest reads containerimage.digest from BuildKit's metadata file.
func readDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read build metadata: %w", err)
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse build metadata: %w", err)
	}
	if m.Digest == "" {
		return "", errors.New("the build produced no image digest")
	}
	return m.Digest, nil
}

// gateName is the file whose appearance lets the daemon start. The agent
// creates it once the build's network is firewalled (ADR 0021).
const gateName = "isolated"

// gateWait is how long the daemon waits for the gate, in seconds.
const gateWait = 300

// daemonSpec is the container of a build's daemon. Its network namespace is the
// container's own, never the host's: containerd pins one per container
// (setNetNS). RootlessKit runs with --net=host, which means "share the
// container's namespace", and the agent firewalls that namespace before the
// daemon starts: the daemon's shell waits for the gate file (ADR 0021).
//
// It is the only container that clears no_new_privs (AllowPrivilegeEscalation):
// RootlessKit runs newuidmap to map the user namespace, and newuidmap needs its
// file capabilities. There are no added capabilities, no devices, no privileged
// mode, and the seccomp and AppArmor settings are the runtime defaults.
func daemonSpec(job Job, id string) runtime.ContainerSpec {
	gate := mount + "/" + gateName
	script := fmt.Sprintf(`i=0; while [ ! -e %s ]; do i=$((i+1)); [ "$i" -gt %d ] && exit 1; sleep 1; done; exec rootlesskit --net=host buildkitd "$@"`, gate, gateWait)
	return runtime.ContainerSpec{
		ID:    id,
		Image: Image,
		// The process sandbox is off (--oci-worker-no-process-sandbox): see the
		// ADR for the trade-off.
		Command: []string{"sh", "-c", script, "sh",
			"--addr", "unix://" + socket, "--root", mount + "/state/root", "--config", mount + "/buildkitd.toml", "--oci-worker-no-process-sandbox"},
		Env:                      []string{"XDG_RUNTIME_DIR=" + mount + "/state/xdg"},
		Mounts:                   []runtime.Mount{{Source: job.Dir, Destination: mount, ReadWrite: true}},
		Labels:                   map[string]string{LabelBuild: job.Build},
		AllowPrivilegeEscalation: true,
	}
}

// privateRanges are the destinations a build cannot reach: loopback, private
// networks (the cluster's pod and service networks, the WireGuard mesh among
// them), carrier-grade NAT and link-local (cloud metadata services).
var privateRanges = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10", "169.254.0.0/16",
}

// isolate firewalls the network namespace of a build's daemon. The daemon's
// RUN steps use that namespace too, so the rules hold for them. It runs from
// the host with nsenter, because the container itself has no CAP_NET_ADMIN.
func isolate(ctx context.Context, rt runtime.Runtime, id, registry string) error {
	nsr, ok := rt.(runtime.NetNSer)
	if !ok {
		return errors.New("the runtime cannot name the container's network namespace")
	}
	path, err := nsr.NetNSPath(ctx, id)
	if err != nil {
		return err
	}
	// An empty registry means no registry exception: the namespace reaches
	// only loopback, DNS and the public internet.
	var regIP net.IP
	var regPort string
	if registry != "" {
		if regIP, regPort, err = registryEndpoint(registry); err != nil {
			return err
		}
	}
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, argv := range isolationRules(regIP, regPort, hostAddresses()) {
		args := append([]string{"--net=" + path}, argv...)
		if _, err := hostOutput(ctx, env, "nsenter", args...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// isolationRules returns the iptables and ip6tables commands, in order, that
// the build's namespace gets. The first match decides:
//
//   - loopback and DNS (port 53, to any resolver) are accepted;
//   - the registry endpoint is accepted, because builds push to it;
//   - every address of the node is rejected, and so is every private range;
//   - all other IPv4 destinations (the public internet) are accepted;
//   - all IPv6 is rejected, except loopback.
//
// Each rule starts with the binary name, and the iptables flags follow it.
func isolationRules(regIP net.IP, regPort string, nodeAddrs []net.IP) [][]string {
	var rules [][]string
	add := func(bin string, args ...string) {
		rules = append(rules, append([]string{bin, "-w", "-A", "OUTPUT"}, args...))
	}
	add("iptables", "-o", "lo", "-j", "ACCEPT")
	add("iptables", "-p", "udp", "--dport", "53", "-j", "ACCEPT")
	add("iptables", "-p", "tcp", "--dport", "53", "-j", "ACCEPT")
	if regIP != nil {
		add("iptables", "-d", regIP.String(), "-p", "tcp", "--dport", regPort, "-j", "ACCEPT")
	}
	for _, ip := range nodeAddrs {
		add("iptables", "-d", ip.String(), "-j", "REJECT")
	}
	for _, c := range privateRanges {
		add("iptables", "-d", c, "-j", "REJECT")
	}
	add("ip6tables", "-o", "lo", "-j", "ACCEPT")
	add("ip6tables", "-j", "REJECT")
	return rules
}

// registryEndpoint splits the registry address into an IPv4 address and a port.
func registryEndpoint(addr string) (net.IP, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("registry address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, "", fmt.Errorf("resolve the registry %s: %w", host, err)
		}
		for _, c := range ips {
			if c.To4() != nil {
				ip = c
				break
			}
		}
	}
	if ip == nil || ip.To4() == nil {
		return nil, "", fmt.Errorf("registry %s has no IPv4 address", host)
	}
	return ip.To4(), port, nil
}

// hostAddresses returns the IPv4 addresses of this node, except loopback and
// link-local ones (the private ranges already cover those).
func hostAddresses() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// cleanup stops and removes the daemon. It runs after ctx may be cancelled.
func cleanup(rt runtime.Runtime, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout+30*time.Second)
	defer cancel()
	_ = rt.Stop(ctx, id, stopTimeout)
	_ = rt.Remove(ctx, id)
}

// containerID is the daemon's container for a Build.
func containerID(build string) string { return "shoal-build-" + build }

// chownTree gives every file and directory under dir to uid.
func chownTree(dir string, uid int) error {
	return filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, uid)
	})
}

// runHost runs a command on the host with env, appending its output to log.
// The error never includes the environment.
func runHost(ctx context.Context, env []string, log io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// hostOutput runs a command on the host and returns its stdout.
func hostOutput(ctx context.Context, env []string, name string, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// redactURL drops the user part of a URL for messages.
func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return u[:i+3] + rest[at+1:]
		}
	}
	return u
}
