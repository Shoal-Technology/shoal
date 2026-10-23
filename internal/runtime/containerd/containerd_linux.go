package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/logfile"
)

// Labels the runtime keeps on each containerd container, so status survives
// agent restarts without any local database.
const (
	labelStarts    = "shoal.runtime.starts"
	labelStartedAt = "shoal.runtime.started-at"
	labelIP        = "shoal.runtime.ip"
	labelPorts     = "shoal.runtime.ports"
	labelImage     = "shoal.runtime.image"
	labelStopReq   = "shoal.runtime.stop-requested" // "true" between Stop and the next Start
	labelNetNS     = "shoal.runtime.netns"          // pinned network namespace (see ensureNetwork)

	cpuPeriod   = 100_000 // CFS period in µs
	cleanupTime = 30 * time.Second
)

type containerdRuntime struct {
	client      *client.Client
	opts        Options
	net         *network
	snapshotter string
}

var _ runtime.Runtime = (*containerdRuntime)(nil)

// New connects to containerd and prepares the state directory and network.
func New(ctx context.Context, o Options) (Client, error) {
	o.setDefaults()
	if o.LogDriverPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve log driver path: %w", err)
		}
		o.LogDriverPath = exe
	}
	for _, d := range []string{"containers", "logs", "netns"} {
		if err := os.MkdirAll(filepath.Join(o.StateDir, d), 0o700); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
	}
	nw, err := newNetwork(o)
	if err != nil {
		return nil, err
	}
	c, err := client.New(o.Address, client.WithDefaultNamespace(o.Namespace), client.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", o.Address, err)
	}
	if _, err := c.Version(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("containerd at %s not responding: %w", o.Address, err)
	}
	return &containerdRuntime{client: c, opts: o, net: nw, snapshotter: defaults.DefaultSnapshotter}, nil
}

func (r *containerdRuntime) Close() error { return r.client.Close() }

func normalizeImage(ref string) (string, error) {
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	return named.String(), nil
}

// imageName is the containerd name of ref. The shoal.local alias is mapped to
// the embedded registry first (ADR 0020, pass B), so pulls and lookups agree.
func (r *containerdRuntime) imageName(ref string) (string, error) {
	return normalizeImage(r.opts.Registry.MapImage(ref))
}

func (r *containerdRuntime) EnsureImage(ctx context.Context, ref string, auth []runtime.RegistryAuth) error {
	name, err := r.imageName(ref)
	if err != nil {
		return err
	}
	img, err := r.client.GetImage(ctx, name)
	switch {
	case err == nil:
		ok, err := img.IsUnpacked(ctx, r.snapshotter)
		if err != nil {
			return fmt.Errorf("check image %s: %w", name, err)
		}
		if !ok {
			if err := img.Unpack(ctx, r.snapshotter); err != nil {
				return fmt.Errorf("unpack image %s: %w", name, err)
			}
		}
		return nil
	case !errdefs.IsNotFound(err):
		return fmt.Errorf("get image %s: %w", name, err)
	}
	opts := []client.RemoteOpt{client.WithPullUnpack, client.WithPullSnapshotter(r.snapshotter)}
	if len(auth) > 0 || r.opts.Registry != nil {
		opts = append(opts, client.WithResolver(pullResolver(auth, r.opts.Registry)))
	}
	if _, err := r.client.Pull(ctx, name, opts...); err != nil {
		return fmt.Errorf("pull image %s: %w", name, err)
	}
	return nil
}

// pullResolver returns a resolver for a pull; see registryHosts.
func pullResolver(auth []runtime.RegistryAuth, reg *runtime.Registry) remotes.Resolver {
	return docker.NewResolver(docker.ResolverOptions{Hosts: registryHosts(auth, reg)})
}

// registryHosts decides how each registry host is reached. Every host logs in
// with the logins in auth (anonymously when there is none). The embedded
// registry reg is the exception: it is reached over TLS with the cluster CA and
// the node's client certificate, with no password (ADR 0020, pass B). No host
// is reached without verification.
func registryHosts(auth []runtime.RegistryAuth, reg *runtime.Registry) docker.RegistryHosts {
	creds := func(host string) (string, string, error) {
		user, pass := runtime.Credentials(auth, host)
		return user, pass, nil
	}
	authorizer := docker.WithAuthorizer(docker.NewDockerAuthorizer(docker.WithAuthCreds(creds)))
	defaults := docker.ConfigureDefaultRegistries(authorizer)
	if reg == nil {
		return defaults
	}
	shoal := docker.ConfigureDefaultRegistries(authorizer, docker.WithClient(reg.HTTPClient()))
	return func(host string) ([]docker.RegistryHost, error) {
		if host == reg.Address {
			return shoal(host)
		}
		return defaults(host)
	}
}

func (r *containerdRuntime) containerDir(id string) string {
	return filepath.Join(r.opts.StateDir, "containers", id)
}

func (r *containerdRuntime) logPath(id string) string {
	return filepath.Join(r.opts.StateDir, "logs", id+".log")
}

func (r *containerdRuntime) Create(ctx context.Context, spec runtime.ContainerSpec) error {
	if _, err := r.client.LoadContainer(ctx, spec.ID); err == nil {
		return fmt.Errorf("create %s: %w", spec.ID, runtime.ErrAlreadyExists)
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("create %s: %w", spec.ID, err)
	}
	name, err := r.imageName(spec.Image)
	if err != nil {
		return err
	}
	img, err := r.client.GetImage(ctx, name)
	if err != nil {
		return fmt.Errorf("create %s: image %s: %w", spec.ID, name, err)
	}

	mounts, err := r.writeEtcFiles(spec.ID)
	if err != nil {
		return fmt.Errorf("create %s: %w", spec.ID, err)
	}
	// Private cgroup namespace plus a read-only cgroup mount (Docker's cgroup
	// v2 default): the container sees only its own cgroup, so runtimes that
	// size themselves from cgroup limits (JVM, Go, Node) work correctly.
	mounts = append(mounts, specs.Mount{
		Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup",
		Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
	})
	mounts = append(mounts, bindMounts(spec.Mounts)...)
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithHostname(spec.ID),
		oci.WithMounts(mounts),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.CgroupNamespace}),
	}
	if len(spec.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(spec.Command...))
	}
	if len(spec.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(spec.Env))
	}
	if spec.MemoryBytes > 0 {
		specOpts = append(specOpts, oci.WithMemoryLimit(uint64(spec.MemoryBytes)))
	}
	if spec.CPUMillis > 0 {
		specOpts = append(specOpts, oci.WithCPUCFS(spec.CPUMillis*cpuPeriod/1000, cpuPeriod))
	}
	specOpts = append(specOpts, privilegeOpts(spec)...)

	ports, err := json.Marshal(spec.Ports)
	if err != nil {
		return fmt.Errorf("create %s: encode ports: %w", spec.ID, err)
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelPorts] = string(ports)
	labels[labelImage] = spec.Image
	labels[labelStarts] = "0"

	// A previous failed Create may have left the snapshot behind.
	if err := r.client.SnapshotService(r.snapshotter).Remove(ctx, spec.ID); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("create %s: remove stale snapshot: %w", spec.ID, err)
	}
	_, err = r.client.NewContainer(ctx, spec.ID,
		client.WithImage(img),
		client.WithNewSnapshot(spec.ID, img),
		client.WithNewSpec(specOpts...),
		client.WithContainerLabels(labels),
	)
	if err != nil {
		return fmt.Errorf("create %s: %w", spec.ID, err)
	}
	return nil
}

// writeEtcFiles generates /etc/resolv.conf and /etc/hosts for the container.
func (r *containerdRuntime) writeEtcFiles(id string) ([]specs.Mount, error) {
	dir := r.containerDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create container dir: %w", err)
	}
	var resolv []byte
	if r.opts.DNSServer != "" {
		// The agent's resolver: Service names first, everything else is
		// forwarded to the host's resolvers.
		resolv = []byte(fmt.Sprintf("nameserver %s\nsearch %s\noptions ndots:1\n", r.opts.DNSServer, strings.Join(r.opts.DNSSearch, " ")))
	} else {
		// systemd-resolved's stub (127.0.0.53) is unreachable from a
		// container netns, so prefer the upstream resolver list.
		var err error
		if resolv, err = os.ReadFile("/run/systemd/resolve/resolv.conf"); err != nil {
			if resolv, err = os.ReadFile("/etc/resolv.conf"); err != nil {
				return nil, fmt.Errorf("read host resolv.conf: %w", err)
			}
		}
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	files := map[string][]byte{"resolv.conf": resolv, "hosts": []byte(hosts)}
	var mounts []specs.Mount
	for _, name := range []string{"resolv.conf", "hosts"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, files[name], 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
		mounts = append(mounts, specs.Mount{
			Destination: "/etc/" + name, Type: "bind", Source: p, Options: []string{"rbind", "ro"},
		})
	}
	return mounts, nil
}

func (r *containerdRuntime) Start(ctx context.Context, id string) (retErr error) {
	c, err := r.load(ctx, id)
	if err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	labels, err := c.Labels(ctx)
	if err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	ports := decodePorts(labels)

	// Remove a previous (exited or half-created) task. Its network stays:
	// it lives in the container's pinned netns.
	if t, err := c.Task(ctx, nil); err == nil {
		st, err := t.Status(ctx)
		if err != nil {
			return fmt.Errorf("start %s: task status: %w", id, err)
		}
		if st.Status == client.Running || st.Status == client.Paused || st.Status == client.Pausing {
			return nil
		}
		if _, err := t.Delete(ctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("start %s: delete old task: %w", id, err)
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("start %s: load task: %w", id, err)
	}
	nsPath, ip, err := r.ensureNetwork(ctx, c, labels, ports)
	if err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	if err := useNetNS(ctx, c, nsPath); err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}

	t, err := c.NewTask(ctx, cio.BinaryIO(r.opts.LogDriverPath, map[string]string{LogDriverFlag: r.logPath(id)}))
	if err != nil {
		return fmt.Errorf("start %s: create task: %w", id, err)
	}
	started := false
	defer func() {
		if retErr == nil || started {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
		defer cancel()
		if _, err := t.Delete(cctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup task: %w", err))
		}
	}()

	if err := t.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", id, err)
	}
	// The process is running now; a label failure must not tear it down.
	started = true
	starts, _ := strconv.Atoi(labels[labelStarts])
	if _, err := c.SetLabels(ctx, map[string]string{
		labelStarts:    strconv.Itoa(starts + 1),
		labelStartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		labelIP:        ip,
		labelStopReq:   "false",
	}); err != nil {
		return fmt.Errorf("start %s: container running but recording start failed: %w", id, err)
	}
	return nil
}

func (r *containerdRuntime) netnsDir() string { return filepath.Join(r.opts.StateDir, "netns") }

// ensureNetwork returns the container's pinned network namespace and IP,
// creating and attaching them on first start. The netns outlives the
// container's process, so restarts keep the IP, and teardown can always
// enter it (the CNI bridge plugin only removes its iptables rules then).
func (r *containerdRuntime) ensureNetwork(ctx context.Context, c client.Container, labels map[string]string, ports []runtime.PortMapping) (string, string, error) {
	id := c.ID()
	if path := labels[labelNetNS]; path != "" {
		closed, err := netns.LoadNetNS(path).Closed()
		if err == nil && !closed && labels[labelIP] != "" {
			return path, labels[labelIP], nil // set up by an earlier start
		}
		// The netns is gone (host reboot: its iptables rules went with it)
		// or a setup didn't finish: release what CNI still holds, start over.
		if err != nil || closed {
			path = ""
		}
		if err := r.net.teardown(ctx, id, path, ports); err != nil {
			return "", "", err
		}
		if err := netns.LoadNetNS(labels[labelNetNS]).Remove(); err != nil {
			return "", "", fmt.Errorf("remove stale network namespace: %w", err)
		}
	}
	// Rules left from a lost netns (or an older version) would make the
	// next teardown of this container fail.
	if err := r.net.purgeMasquerade(ctx, id); err != nil {
		return "", "", err
	}
	ns, err := netns.NewNetNS(r.netnsDir())
	if err != nil {
		return "", "", fmt.Errorf("create network namespace: %w", err)
	}
	// Record the netns before attaching it, so a crash in between leaves a
	// trace that the next start or Remove cleans up.
	if _, err := c.SetLabels(ctx, map[string]string{labelNetNS: ns.GetPath(), labelIP: ""}); err != nil {
		return "", "", errors.Join(fmt.Errorf("record network namespace: %w", err), ns.Remove())
	}
	ip, err := r.net.setup(ctx, id, ns.GetPath(), ports)
	if err != nil {
		return "", "", err
	}
	if _, err := c.SetLabels(ctx, map[string]string{labelIP: ip}); err != nil {
		return "", "", fmt.Errorf("record IP: %w", err)
	}
	return ns.GetPath(), ip, nil
}

// releaseNetwork detaches the container from CNI and removes its netns.
func (r *containerdRuntime) releaseNetwork(ctx context.Context, id string, labels map[string]string) error {
	path := labels[labelNetNS]
	attached := ""
	if path != "" {
		if closed, err := netns.LoadNetNS(path).Closed(); err == nil && !closed {
			attached = path
		}
	}
	if err := r.net.teardown(ctx, id, attached, decodePorts(labels)); err != nil {
		return err
	}
	if attached == "" {
		// The plugin couldn't enter the netns, so it left its iptables rules.
		if err := r.net.purgeMasquerade(ctx, id); err != nil {
			return err
		}
	}
	if path != "" {
		if err := netns.LoadNetNS(path).Remove(); err != nil {
			return fmt.Errorf("remove network namespace: %w", err)
		}
	}
	return nil
}

// useNetNS points the container's spec at the pinned netns.
func useNetNS(ctx context.Context, c client.Container, path string) error {
	spec, err := c.Spec(ctx)
	if err != nil {
		return fmt.Errorf("load spec: %w", err)
	}
	if !setNetNS(spec, path) {
		return nil
	}
	if err := c.Update(ctx, client.UpdateContainerOpts(client.WithSpec(spec))); err != nil {
		return fmt.Errorf("set network namespace: %w", err)
	}
	return nil
}

// setNetNS makes the spec's network namespace the one at path, which is a
// namespace of the container and never the host's: a spec without a network
// namespace entry runs in the host's (ADR 0021). It reports whether the spec
// changed.
func setNetNS(spec *specs.Spec, path string) bool {
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	found := false
	for i, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			if ns.Path == path {
				return false
			}
			spec.Linux.Namespaces[i].Path, found = path, true
		}
	}
	if !found {
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: path})
	}
	return true
}

func (r *containerdRuntime) Stop(ctx context.Context, id string, timeout time.Duration) error {
	c, err := r.load(ctx, id)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	t, err := c.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stop %s: load task: %w", id, err)
	}
	st, err := t.Status(ctx)
	if err != nil {
		return fmt.Errorf("stop %s: task status: %w", id, err)
	}
	switch st.Status {
	case client.Stopped:
		return nil
	case client.Created:
		// Never started: nothing to signal, just discard it.
		if _, err := t.Delete(ctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("stop %s: delete task: %w", id, err)
		}
		return nil
	}

	// Record the request before signalling, so the exit is never mistaken for
	// a crash (or a clean exit) of the application itself.
	if _, err := c.SetLabels(ctx, map[string]string{labelStopReq: "true"}); err != nil {
		return fmt.Errorf("stop %s: record stop request: %w", id, err)
	}
	exited, err := t.Wait(ctx)
	if err != nil {
		return fmt.Errorf("stop %s: wait: %w", id, err)
	}
	if err := t.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop %s: SIGTERM: %w", id, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop %s: %w", id, ctx.Err())
	case <-timer.C:
	}
	if err := t.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop %s: SIGKILL: %w", id, err)
	}
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop %s: %w", id, ctx.Err())
	}
}

func (r *containerdRuntime) Remove(ctx context.Context, id string) error {
	c, err := r.load(ctx, id)
	if errors.Is(err, runtime.ErrNotFound) {
		return r.removeFiles(id)
	} else if err != nil {
		return fmt.Errorf("remove %s: %w", id, err)
	}
	labels, err := c.Labels(ctx)
	if err != nil {
		return fmt.Errorf("remove %s: %w", id, err)
	}
	if t, err := c.Task(ctx, nil); err == nil {
		if _, err := t.Delete(ctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove %s: delete task: %w", id, err)
		}
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: load task: %w", id, err)
	}
	if err := r.releaseNetwork(ctx, id, labels); err != nil {
		return fmt.Errorf("remove %s: %w", id, err)
	}
	if err := c.Delete(ctx, client.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", id, err)
	}
	return r.removeFiles(id)
}

func (r *containerdRuntime) removeFiles(id string) error {
	if err := os.RemoveAll(r.containerDir(id)); err != nil {
		return fmt.Errorf("remove %s files: %w", id, err)
	}
	if err := logfile.Remove(r.logPath(id), LogMaxFiles); err != nil {
		return fmt.Errorf("remove %s logs: %w", id, err)
	}
	return nil
}

// privilegeOpts returns the spec changes that let a container gain privileges
// through setuid binaries and file capabilities. Only a container that asks for
// AllowPrivilegeEscalation gets them; every other container keeps containerd's
// default of no_new_privs (ADR 0021).
func privilegeOpts(spec runtime.ContainerSpec) []oci.SpecOpts {
	if !spec.AllowPrivilegeEscalation {
		return nil
	}
	return []oci.SpecOpts{oci.WithNewPrivileges}
}

// NetNSPath returns the pinned network namespace of a container, which exists
// once the container has been started (ensureNetwork). The builder applies its
// firewall there (ADR 0021).
func (r *containerdRuntime) NetNSPath(ctx context.Context, id string) (string, error) {
	c, err := r.load(ctx, id)
	if err != nil {
		return "", err
	}
	labels, err := c.Labels(ctx)
	if err != nil {
		return "", err
	}
	if labels[labelNetNS] == "" {
		return "", fmt.Errorf("container %s has no network namespace yet", id)
	}
	return labels[labelNetNS], nil
}

func (r *containerdRuntime) Status(ctx context.Context, id string) (runtime.ContainerStatus, error) {
	c, err := r.load(ctx, id)
	if err != nil {
		return runtime.ContainerStatus{}, fmt.Errorf("status %s: %w", id, err)
	}
	return r.status(ctx, c)
}

func (r *containerdRuntime) status(ctx context.Context, c client.Container) (runtime.ContainerStatus, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return runtime.ContainerStatus{}, fmt.Errorf("status %s: %w", c.ID(), err)
	}
	starts, _ := strconv.Atoi(info.Labels[labelStarts])
	startedAt, _ := time.Parse(time.RFC3339Nano, info.Labels[labelStartedAt])
	st := runtime.ContainerStatus{
		ID:        c.ID(),
		Image:     info.Labels[labelImage],
		Labels:    userLabels(info.Labels),
		Starts:    starts,
		StartedAt: startedAt,
		IP:        info.Labels[labelIP],

		StopRequested: info.Labels[labelStopReq] == "true",
	}

	t, err := c.Task(ctx, nil)
	switch {
	case errdefs.IsNotFound(err):
		// No task: never started, or lost (e.g. host reboot).
		if starts == 0 {
			st.State = runtime.StateCreated
		} else {
			st.State, st.ExitCode, st.IP = runtime.StateExited, -1, ""
		}
		return st, nil
	case err != nil:
		return runtime.ContainerStatus{}, fmt.Errorf("status %s: load task: %w", c.ID(), err)
	}
	ts, err := t.Status(ctx)
	if err != nil {
		return runtime.ContainerStatus{}, fmt.Errorf("status %s: task status: %w", c.ID(), err)
	}
	switch ts.Status {
	case client.Running, client.Paused, client.Pausing:
		st.State = runtime.StateRunning
	case client.Created:
		st.State = runtime.StateCreated
	case client.Stopped:
		st.State, st.ExitCode, st.FinishedAt, st.IP = runtime.StateExited, int(ts.ExitStatus), ts.ExitTime, ""
	default:
		st.State = runtime.StateUnknown
	}
	return st, nil
}

func (r *containerdRuntime) List(ctx context.Context) ([]runtime.ContainerStatus, error) {
	cs, err := r.client.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]runtime.ContainerStatus, 0, len(cs))
	for _, c := range cs {
		st, err := r.status(ctx, c)
		if errdefs.IsNotFound(err) {
			continue // removed concurrently
		}
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (r *containerdRuntime) Logs(ctx context.Context, id string, opts runtime.LogOptions, fn func(runtime.LogEntry) error) error {
	c, err := r.load(ctx, id)
	if err != nil {
		return fmt.Errorf("logs %s: %w", id, err)
	}
	// A follow ends when the task exits. Without a task there is nothing to
	// follow; a stopped task's Wait returns at once.
	var stop chan struct{}
	if opts.Follow {
		t, err := c.Task(ctx, nil)
		switch {
		case errdefs.IsNotFound(err):
			opts.Follow = false
		case err != nil:
			return fmt.Errorf("logs %s: load task: %w", id, err)
		default:
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			exited, err := t.Wait(wctx)
			if err != nil {
				return fmt.Errorf("logs %s: wait: %w", id, err)
			}
			stop = make(chan struct{})
			go func() {
				select {
				case <-exited:
					close(stop)
				case <-wctx.Done():
				}
			}()
		}
	}
	if err := logfile.Read(ctx, r.logPath(id), LogMaxFiles, opts, stop, fn); err != nil {
		return fmt.Errorf("logs %s: %w", id, err)
	}
	return nil
}

var execSeq atomic.Uint64

// Exec runs argv in the running container as a separate process. The process
// takes the container's process spec (user, environment, working directory)
// and is deleted afterwards. Not exercised until the e2e pass (ADR 0016, F).
func (r *containerdRuntime) Exec(ctx context.Context, id string, argv []string, timeout time.Duration, opts ...runtime.ExecOption) (res runtime.ExecResult, retErr error) {
	if len(argv) == 0 {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: empty command", id)
	}
	c, err := r.load(ctx, id)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: %w", id, err)
	}
	t, err := c.Task(ctx, nil)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: no task: %w", id, err)
	}
	ts, err := t.Status(ctx)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: task status: %w", id, err)
	}
	if ts.Status != client.Running {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: container is not running", id)
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: load spec: %w", id, err)
	}
	if spec.Process == nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: container has no process spec", id)
	}
	proc := *spec.Process
	proc.Args = argv
	proc.Terminal = false
	proc.Env = append(append([]string(nil), proc.Env...), runtime.ExecEnv(opts...)...)

	stdout := &runtime.CappedBuffer{Limit: runtime.ExecOutputLimit}
	stderr := &runtime.CappedBuffer{Limit: runtime.ExecOutputLimit}
	execID := fmt.Sprintf("shoal-exec-%d-%d", time.Now().UnixNano(), execSeq.Add(1))
	p, err := t.Exec(ctx, execID, &proc, cio.NewCreator(cio.WithStreams(nil, stdout, stderr)))
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: %w", id, err)
	}
	// Remove the exec process on every path, including a failed start.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
		defer cancel()
		if _, err := p.Delete(cctx, client.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			retErr = errors.Join(retErr, fmt.Errorf("exec %s: delete process: %w", id, err))
		}
	}()

	exitCh, err := p.Wait(ctx)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: wait: %w", id, err)
	}
	if err := p.Start(ctx); err != nil {
		return runtime.ExecResult{}, fmt.Errorf("exec %s: start: %w", id, err)
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	res.ExitCode = -1
	select {
	case st := <-exitCh:
		code, _, err := st.Result()
		if err != nil {
			return res, fmt.Errorf("exec %s: exit status: %w", id, err)
		}
		res.ExitCode = int(code)
	case <-expired:
		_ = p.Kill(ctx, syscall.SIGKILL)
		select {
		case <-exitCh:
		case <-ctx.Done():
		}
		return finishExec(res, stdout, stderr), fmt.Errorf("exec %s: timed out after %s", id, timeout)
	case <-ctx.Done():
		_ = p.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		return runtime.ExecResult{}, fmt.Errorf("exec %s: %w", id, ctx.Err())
	}
	return finishExec(res, stdout, stderr), nil
}

// finishExec copies the captured output into res.
func finishExec(res runtime.ExecResult, stdout, stderr *runtime.CappedBuffer) runtime.ExecResult {
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	res.Truncated = stdout.Truncated() || stderr.Truncated()
	return res
}

func (r *containerdRuntime) load(ctx context.Context, id string) (client.Container, error) {
	c, err := r.client.LoadContainer(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil, runtime.ErrNotFound
	}
	return c, err
}

func decodePorts(labels map[string]string) []runtime.PortMapping {
	var ports []runtime.PortMapping
	_ = json.Unmarshal([]byte(labels[labelPorts]), &ports) // absent/invalid = no ports
	return ports
}

// userLabels strips the runtime's internal labels.
func userLabels(all map[string]string) map[string]string {
	out := make(map[string]string, len(all))
	for k, v := range all {
		switch k {
		case labelStarts, labelStartedAt, labelIP, labelPorts, labelImage, labelStopReq, labelNetNS:
		default:
			out[k] = v
		}
	}
	return out
}
