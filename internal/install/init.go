package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

const dirBin = "/usr/local/bin"

// Options configures Init. Binary is the shoal executable to install; the
// command entry point reads it from its own path.
type Options struct {
	DryRun        bool
	Email         string
	AcceptTOS     bool
	NodeName      string
	Edge          bool
	Swap          string
	OpenFirewall  bool
	Join          *Bundle
	Binary        []byte
	UserName      string
	UserHome      string
	ACMEDirectory string // test only
	ACMECARoots   string // test only
	Out           io.Writer
}

// Step is one idempotent unit of the plan. Check reports whether the step is
// already done (and reads only). Describe is the action for a dry run or a
// real run; Ok is the reason when nothing is needed.
type Step struct {
	Name     string
	Skip     func(e *env) string
	Check    func(ctx context.Context, e *env) (bool, error)
	Ok       func(e *env) string
	Describe func(e *env) string
	Apply    func(ctx context.Context, e *env) error
}

type env struct {
	h       Host
	o       Options
	f       Facts
	rec     Record
	out     io.Writer
	changed bool
}

// Init runs the install plan (ADR 0017, 1.2). A dry run prints every step and
// changes nothing. Errors are *ExitError with the exit code.
func Init(ctx context.Context, h Host, o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Swap != "" {
		if _, err := parseSizeMiB(o.Swap); err != nil {
			return err
		}
	}
	if o.Join != nil && o.Join.Format != BundleFormat {
		return fmt.Errorf("join bundle: format %q", o.Join.Format)
	}
	f, err := h.Facts(ctx)
	if err != nil {
		return &ExitError{Code: 1, Err: fmt.Errorf("preflight: %w", err)}
	}
	failed := false
	for _, c := range Preflight(f, o.Join != nil) {
		fmt.Fprintln(o.Out, c.Line())
		failed = failed || c.Fail()
	}
	if failed {
		return &ExitError{Code: 2, Err: errors.New("preflight failed; nothing was changed")}
	}
	rec, _, err := loadRecord(ctx, h)
	if err != nil {
		return &ExitError{Code: 1, Err: err}
	}
	e := &env{h: h, o: o, f: f, rec: rec, out: o.Out}
	for _, s := range e.steps() {
		if err := e.runStep(ctx, s); err != nil {
			code := 1
			if e.changed {
				code = 3
			}
			return &ExitError{Code: code, Err: fmt.Errorf("step %s: %w", s.Name, err)}
		}
	}
	if o.DryRun {
		fmt.Fprintln(o.Out, "dry run: nothing was changed")
		return nil
	}
	e.nextSteps()
	return nil
}

func (e *env) say(tag, name, detail string) {
	fmt.Fprintf(e.out, "%-6s %s: %s\n", tag, name, detail)
}

func (e *env) runStep(ctx context.Context, s Step) error {
	if s.Skip != nil {
		if why := s.Skip(e); why != "" {
			e.say("skip", s.Name, why)
			return nil
		}
	}
	done, err := s.Check(ctx, e)
	if err != nil {
		return err
	}
	if done {
		e.say("ok", s.Name, s.Ok(e))
		return nil
	}
	if e.o.DryRun {
		e.say("WOULD", s.Name, s.Describe(e))
		return nil
	}
	if err := s.Apply(ctx, e); err != nil {
		return err
	}
	e.say("done", s.Name, s.Describe(e))
	return saveRecord(e.h, e.rec)
}

func (e *env) steps() []Step {
	return []Step{
		kernelStep(),
		swapStep(),
		binaryStep(),
		containerdStep(),
		cniStep(),
		containerdServiceStep(),
		serverUnitStep(),
		serverReadyStep(),
		agentStep(),
		nodeReadyStep(),
		edgeStep(),
		joinStep(),
		adminStep(),
		firewallStep(),
	}
}

func (e *env) join() bool { return e.o.Join != nil }

func (e *env) node() string {
	if e.o.NodeName != "" {
		return e.o.NodeName
	}
	return e.f.Hostname
}

// containerdOwned: init installed it, or it will (no containerd on the host).
func (e *env) containerdOwned() bool {
	return e.rec.ContainerdOwned || e.f.ContainerdVersion == ""
}

// reuseContainerd: a 2.x containerd that init did not install is left as it is.
func (e *env) reuseContainerd() bool {
	return e.f.ContainerdVersion != "" && !e.rec.ContainerdOwned
}

func (e *env) adminCopy() string { return path.Join(e.o.UserHome, ".shoal", "config") }

func (e *env) fileIs(p string, want []byte) bool {
	got, err := e.h.ReadFile(p)
	return err == nil && bytes.Equal(got, want)
}

// write writes a file and records it in the install record when init creates it.
func (e *env) write(p string, data []byte, mode fs.FileMode) error {
	created := !e.h.Exists(p)
	if err := e.h.WriteFile(p, data, mode); err != nil {
		return err
	}
	e.changed = true
	if created && !contains(e.rec.Owned, p) {
		e.rec.Owned = append(e.rec.Owned, p)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (e *env) active(ctx context.Context, unit string) bool {
	_, err := e.h.Run(ctx, "systemctl", "is-active", "--quiet", unit)
	return err == nil
}

// ensureUnit writes a unit file when its content differs, reloads systemd,
// enables the unit, and starts it (or restarts it after a change).
func (e *env) ensureUnit(ctx context.Context, unit, p, content string) error {
	changed := !e.fileIs(p, []byte(content))
	if changed {
		if err := e.write(p, []byte(content), 0o644); err != nil {
			return err
		}
		if _, err := e.h.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if _, err := e.h.Run(ctx, "systemctl", "enable", unit); err != nil {
		return err
	}
	active := e.active(ctx, unit)
	switch {
	case active && changed:
		_, err := e.h.Run(ctx, "systemctl", "restart", unit)
		return err
	case !active:
		_, err := e.h.Run(ctx, "systemctl", "start", unit)
		return err
	}
	return nil
}

// shoal runs the installed CLI against the local admin config.
func (e *env) shoal(ctx context.Context, args ...string) (string, error) {
	return e.h.Run(ctx, pathShoalBin, append(args, "--config", pathAdmin, "--server", serverAddr)...)
}

func (e *env) serverAnswers(ctx context.Context) bool {
	if !e.h.Exists(pathAdmin) {
		return false
	}
	_, err := e.shoal(ctx, "get", "nodes")
	return err == nil
}

func (e *env) nodeReady(ctx context.Context) bool {
	out, err := e.shoal(ctx, "get", "nodes")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == e.node() && f[1] == "Ready" {
			return true
		}
	}
	return false
}

// agentUnitText is the agent unit this host should have now.
func (e *env) agentUnitText() string {
	if e.join() {
		return agentUnit(e.node(), e.rec.Server, e.rec.CAHash, false)
	}
	return agentUnit(e.node(), serverAddr, e.rec.CAHash, true)
}

func (e *env) serverUnitText() string {
	return serverUnit(e.o.Email, e.o.AcceptTOS, e.o.ACMEDirectory, e.o.ACMECARoots)
}

func (e *env) acmeNote() string {
	switch {
	case e.o.Email != "" && e.o.AcceptTOS:
		return " (ACME on: email given and terms accepted)"
	case e.o.Email != "":
		return " (ACME off until --accept-tos is given)"
	default:
		return " (ACME off: no --email)"
	}
}

func (e *env) nextSteps() {
	out := e.out
	ip := e.f.IPv4
	if ip == "" {
		ip = "this host's public IP"
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "next steps")
	if e.join() {
		fmt.Fprintf(out, "  - Joining does not change DNS. Add %s to your domain's A records if this edge should take traffic.\n", ip)
		if e.o.Edge {
			fmt.Fprintf(out, "  - On a node with admin access, label this node as an edge: shoal label node %s shoal.dev/edge=true\n", e.node())
		}
		return
	}
	fmt.Fprintf(out, "  - DNS: point an A record for your app's domain at %s (this edge).\n", ip)
	fmt.Fprintf(out, "  - Deploy an app: shoal deploy shoal.yaml (admin config: %s)\n", e.adminCopy())
	fmt.Fprintf(out, "  - UI password: %s/ui-initial-password (reset: sudo shoal ui reset-password)\n", dataDirServer)
	fmt.Fprintln(out, "  - Add a node: sudo shoal token create --bundle --out join.json here, then sudo shoal init --join join.json on that host")
}

// waitFor polls ok until it is true or the timeout passes.
func waitFor(ctx context.Context, d time.Duration, what string, ok func() bool) error {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", d, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func skipJoin(e *env) string {
	if e.join() {
		return "join node: this host runs no server"
	}
	return ""
}

func kernelStep() Step {
	return Step{
		Name: "kernel",
		Check: func(ctx context.Context, e *env) (bool, error) {
			if !e.fileIs(pathModules, []byte(modulesConf)) || !e.fileIs(pathSysctl, []byte(sysctlConf)) || !e.f.BrNetfilterLoaded {
				return false, nil
			}
			for _, k := range sysctlKeys {
				if e.f.Sysctl[k] != "1" {
					return false, nil
				}
			}
			return true, nil
		},
		Ok: func(*env) string { return "br_netfilter loaded; ip_forward and bridge-nf-call-iptables are 1" },
		Describe: func(*env) string {
			return fmt.Sprintf("write %s and %s; load br_netfilter; set %s=1 and %s=1",
				pathModules, pathSysctl, sysctlKeys[0], sysctlKeys[1])
		},
		Apply: func(ctx context.Context, e *env) error {
			if err := e.write(pathModules, []byte(modulesConf), 0o644); err != nil {
				return err
			}
			if err := e.write(pathSysctl, []byte(sysctlConf), 0o644); err != nil {
				return err
			}
			if _, err := e.h.Run(ctx, "modprobe", "br_netfilter"); err != nil {
				return err
			}
			for _, k := range sysctlKeys {
				if _, err := e.h.Run(ctx, "sysctl", "-w", k+"=1"); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func swapStep() Step {
	return Step{
		Name: "swap",
		Skip: func(e *env) string {
			if e.o.Swap == "" {
				return "not requested (--swap)"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			mib, err := parseSizeMiB(e.o.Swap)
			if err != nil {
				return false, err
			}
			return e.f.SwapMiB >= mib, nil
		},
		Ok: func(e *env) string { return "swap of " + e.o.Swap + " or more is active" },
		Describe: func(e *env) string {
			return fmt.Sprintf("create /swapfile (%s), mkswap, swapon, add it to /etc/fstab", swapSizeArg(e.o.Swap))
		},
		Apply: func(ctx context.Context, e *env) error {
			size := swapSizeArg(e.o.Swap)
			for _, args := range [][]string{
				{"fallocate", "-l", size, "/swapfile"},
				{"chmod", "600", "/swapfile"},
				{"mkswap", "/swapfile"},
				{"swapon", "/swapfile"},
			} {
				if _, err := e.h.Run(ctx, args[0], args[1:]...); err != nil {
					return err
				}
			}
			fstab, err := e.h.ReadFile("/etc/fstab")
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if !strings.Contains(string(fstab), "/swapfile") {
				fstab = append(fstab, []byte("/swapfile none swap sw 0 0\n")...)
				if err := e.h.WriteFile("/etc/fstab", fstab, 0o644); err != nil {
					return err
				}
			}
			e.rec.SwapFile = true
			e.changed = true
			return nil
		},
	}
}

func binaryStep() Step {
	return Step{
		Name: "binary",
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.fileIs(pathShoalBin, e.o.Binary), nil
		},
		Ok: func(*env) string { return "shoal binary already at " + pathShoalBin },
		Describe: func(e *env) string {
			return fmt.Sprintf("install the shoal binary (%d bytes) to %s", len(e.o.Binary), pathShoalBin)
		},
		Apply: func(ctx context.Context, e *env) error {
			return e.write(pathShoalBin, e.o.Binary, 0o755)
		},
	}
}

func containerdStep() Step {
	return Step{
		Name: "containerd",
		Check: func(ctx context.Context, e *env) (bool, error) {
			if e.reuseContainerd() {
				return true, nil
			}
			return e.rec.ContainerdOwned && e.h.Exists(pathContainerd) && e.h.Exists(pathRunc), nil
		},
		Ok: func(e *env) string {
			if e.reuseContainerd() {
				return "reusing containerd " + e.f.ContainerdVersion + " as it is (not changed)"
			}
			return "containerd " + ContainerdVersion + " and runc " + RuncVersion + " installed by shoal init"
		},
		Describe: func(e *env) string {
			return fmt.Sprintf("install containerd %s and runc %s (%s) from the pinned releases; sha256 is checked before any write",
				ContainerdVersion, RuncVersion, e.f.Arch)
		},
		Apply: func(ctx context.Context, e *env) error {
			ca, err := assetFor(containerdAssets, e.f.Arch)
			if err != nil {
				return err
			}
			ra, err := assetFor(runcAssets, e.f.Arch)
			if err != nil {
				return err
			}
			cdata, err := e.h.Fetch(ctx, ca.URL)
			if err != nil {
				return err
			}
			if err := verifyAsset(ca, cdata); err != nil {
				return err
			}
			rdata, err := e.h.Fetch(ctx, ra.URL)
			if err != nil {
				return err
			}
			if err := verifyAsset(ra, rdata); err != nil {
				return err
			}
			files, err := untarGz(cdata, matchBinDir, dirBin)
			if err != nil {
				return err
			}
			for _, f := range files {
				if err := e.write(f.Dest, f.Data, f.Mode); err != nil {
					return err
				}
			}
			if err := e.write(pathRunc, rdata, 0o755); err != nil {
				return err
			}
			e.rec.ContainerdOwned = true
			e.rec.ContainerdVersion = ContainerdVersion
			return nil
		},
	}
}

func cniStep() Step {
	plugin := func(name string) string { return path.Join(dirCNI, name) }
	return Step{
		Name: "cni",
		Check: func(ctx context.Context, e *env) (bool, error) {
			for _, p := range cniPlugins {
				if !e.h.Exists(plugin(p)) {
					return false, nil
				}
			}
			return true, nil
		},
		Ok: func(*env) string { return "CNI plugins present in " + dirCNI },
		Describe: func(e *env) string {
			return fmt.Sprintf("install CNI plugins v%s (%s) to %s from the pinned release; sha256 is checked", CNIVersion, e.f.Arch, dirCNI)
		},
		Apply: func(ctx context.Context, e *env) error {
			a, err := assetFor(cniAssets, e.f.Arch)
			if err != nil {
				return err
			}
			data, err := e.h.Fetch(ctx, a.URL)
			if err != nil {
				return err
			}
			if err := verifyAsset(a, data); err != nil {
				return err
			}
			files, err := untarGz(data, matchExecutable, dirCNI)
			if err != nil {
				return err
			}
			for _, f := range files {
				if err := e.write(f.Dest, f.Data, f.Mode); err != nil {
					return err
				}
			}
			e.rec.CNIVersion = CNIVersion
			return nil
		},
	}
}

func containerdServiceStep() Step {
	return Step{
		Name: "containerd-service",
		Skip: func(e *env) string {
			if !e.containerdOwned() {
				return "containerd was not installed by shoal init; left as it is"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.fileIs(pathContainerdConfig, []byte(containerdConfig)) &&
				e.fileIs(pathContainerdUnit, []byte(containerdUnit)) &&
				e.active(ctx, unitContainerd), nil
		},
		Ok: func(*env) string { return "containerd config and unit in place; service running" },
		Describe: func(*env) string {
			return fmt.Sprintf("write %s (CRI plugins disabled) and %s; enable and start containerd",
				pathContainerdConfig, pathContainerdUnit)
		},
		Apply: func(ctx context.Context, e *env) error {
			if err := e.write(pathContainerdConfig, []byte(containerdConfig), 0o644); err != nil {
				return err
			}
			return e.ensureUnit(ctx, unitContainerd, pathContainerdUnit, containerdUnit)
		},
	}
}

func serverUnitStep() Step {
	return Step{
		Name: "server-unit",
		Skip: skipJoin,
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.fileIs(pathServerUnit, []byte(e.serverUnitText())) && e.active(ctx, unitServer), nil
		},
		Ok: func(*env) string { return unitServer + " running" },
		Describe: func(e *env) string {
			return fmt.Sprintf("write %s and start %s%s", pathServerUnit, unitServer, e.acmeNote())
		},
		Apply: func(ctx context.Context, e *env) error {
			return e.ensureUnit(ctx, unitServer, pathServerUnit, e.serverUnitText())
		},
	}
}

func serverReadyStep() Step {
	return Step{
		Name: "server-ready",
		Skip: skipJoin,
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.serverAnswers(ctx), nil
		},
		Ok: func(*env) string { return "server answers with admin credentials" },
		Describe: func(*env) string {
			return "wait up to 60s for " + pathAdmin + " and a successful get nodes"
		},
		Apply: func(ctx context.Context, e *env) error {
			return waitFor(ctx, 60*time.Second, "the server", func() bool { return e.serverAnswers(ctx) })
		},
	}
}

func agentStep() Step {
	return Step{
		Name: "agent",
		Check: func(ctx context.Context, e *env) (bool, error) {
			if !e.h.Exists(pathCreds) {
				return false, nil
			}
			return e.fileIs(pathAgentUnit, []byte(e.agentUnitText())) && e.active(ctx, unitAgent), nil
		},
		Ok: func(*env) string { return "agent registered and running" },
		Describe: func(e *env) string {
			if e.join() {
				return fmt.Sprintf("write %s from the join bundle (0600), write %s for server %s, start %s",
					pathJoinToken, pathAgentUnit, e.o.Join.Server, unitAgent)
			}
			return fmt.Sprintf("create a single-use join token for %s (ttl 10m), write %s (0600), write %s, start %s",
				e.node(), pathJoinToken, pathAgentUnit, unitAgent)
		},
		Apply: func(ctx context.Context, e *env) error {
			if e.join() {
				e.rec.Server = e.o.Join.Server
				e.rec.CAHash = e.o.Join.CAHash
				e.rec.Joined = true
				if err := writeJoinToken(e, e.o.Join.Token); err != nil {
					return err
				}
				return e.ensureUnit(ctx, unitAgent, pathAgentUnit, e.agentUnitText())
			}
			if !e.h.Exists(pathCreds) {
				out, err := e.shoal(ctx, "token", "create", "--max-uses", "1", "--ttl", "10m", "--description", "init-local")
				if err != nil {
					return err
				}
				_, token, caHash, err := parseToken(out)
				if err != nil {
					return err
				}
				e.rec.CAHash = caHash
				e.rec.Server = serverAddr
				if err := writeJoinToken(e, token); err != nil {
					return err
				}
			}
			return e.ensureUnit(ctx, unitAgent, pathAgentUnit, e.agentUnitText())
		},
	}
}

func writeJoinToken(e *env, token string) error {
	if err := e.h.MkdirAll(dirRun, 0o700); err != nil {
		return err
	}
	return e.h.WriteFile(pathJoinToken, []byte(token+"\n"), 0o600)
}

func nodeReadyStep() Step {
	return Step{
		Name: "node-ready",
		Skip: func(e *env) string {
			if e.join() {
				return "join node: the node is checked from the cluster"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.nodeReady(ctx), nil
		},
		Ok: func(e *env) string { return "node " + e.node() + " is Ready" },
		Describe: func(e *env) string {
			return "wait up to 60s for node " + e.node() + " to be Ready"
		},
		Apply: func(ctx context.Context, e *env) error {
			return waitFor(ctx, 60*time.Second, "node "+e.node()+" to be Ready", func() bool { return e.nodeReady(ctx) })
		},
	}
}

func edgeStep() Step {
	return Step{
		Name: "edge",
		Skip: func(e *env) string {
			if e.join() {
				return "join node: label it from the cluster (see next steps)"
			}
			if !e.o.Edge {
				return "not requested (--edge=false)"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.rec.EdgeLabeled, nil
		},
		Ok: func(e *env) string { return "node " + e.node() + " carries shoal.dev/edge=true" },
		Describe: func(e *env) string {
			return "label node " + e.node() + " shoal.dev/edge=true (serves ports 80 and 443)"
		},
		Apply: func(ctx context.Context, e *env) error {
			if _, err := e.shoal(ctx, "label", "node", e.node(), "shoal.dev/edge=true"); err != nil {
				return err
			}
			e.rec.EdgeLabeled = true
			return nil
		},
	}
}

func joinStep() Step {
	return Step{
		Name: "join",
		Skip: func(e *env) string {
			if !e.join() {
				return "not a join"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			return e.h.Exists(pathCreds), nil
		},
		Ok: func(*env) string { return "node registered with the cluster" },
		Describe: func(*env) string {
			return "wait up to 60s for the agent to register with the cluster"
		},
		Apply: func(ctx context.Context, e *env) error {
			return waitFor(ctx, 60*time.Second, "the agent to register", func() bool {
				return e.h.Exists(pathCreds)
			})
		},
	}
}

func adminStep() Step {
	return Step{
		Name: "admin-config",
		Skip: skipJoin,
		Check: func(ctx context.Context, e *env) (bool, error) {
			admin, err := e.h.ReadFile(pathAdmin)
			if err != nil {
				return false, nil
			}
			return e.fileIs(e.adminCopy(), admin), nil
		},
		Ok: func(e *env) string { return "admin config in place at " + e.adminCopy() },
		Describe: func(e *env) string {
			return fmt.Sprintf("copy %s to %s (mode 0600, owner %s)", pathAdmin, e.adminCopy(), e.o.UserName)
		},
		Apply: func(ctx context.Context, e *env) error {
			admin, err := e.h.ReadFile(pathAdmin)
			if err != nil {
				return err
			}
			dir := path.Dir(e.adminCopy())
			if err := e.h.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := e.h.WriteFile(e.adminCopy(), admin, 0o600); err != nil {
				return err
			}
			if e.o.UserName != "" {
				if err := e.h.Chown(dir, e.o.UserName); err != nil {
					return err
				}
				return e.h.Chown(e.adminCopy(), e.o.UserName)
			}
			return nil
		},
	}
}

func firewallStep() Step {
	ports := func(e *env) []string {
		p := []string{"80/tcp", "443/tcp"}
		if e.join() {
			p = append(p, "51820/udp")
		}
		return p
	}
	return Step{
		Name: "firewall",
		Skip: func(e *env) string {
			if !e.o.OpenFirewall {
				return "not requested (--open-firewall)"
			}
			return ""
		},
		Check: func(ctx context.Context, e *env) (bool, error) {
			if e.f.UFW != "active" {
				return true, nil
			}
			out, _ := e.h.Run(ctx, "ufw", "status")
			for _, p := range ports(e) {
				if !strings.Contains(out, p) {
					return false, nil
				}
			}
			return true, nil
		},
		Ok: func(e *env) string {
			if e.f.UFW != "active" {
				return "ufw is not active; nothing to open"
			}
			return "ports already allowed in ufw"
		},
		Describe: func(e *env) string {
			return "ufw allow " + strings.Join(ports(e), ", ") + " (ufw is active; it is never enabled here)"
		},
		Apply: func(ctx context.Context, e *env) error {
			for _, p := range ports(e) {
				if _, err := e.h.Run(ctx, "ufw", "allow", p); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
