package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// UninstallOptions configures Uninstall.
type UninstallOptions struct {
	DryRun   bool
	Purge    bool
	Yes      bool
	UserHome string
	Out      io.Writer
}

// Uninstall reverses init (ADR 0017, 1.7). It stops and removes both shoal
// units, the binary, the sysctl and module files and the unit files. It removes
// containerd and CNI only when the install record says init installed them, and
// only when no containers outside the shoal namespace are running. /var/lib/shoal
// is kept unless Purge and Yes are given. ~/.shoal/config is never removed.
func Uninstall(ctx context.Context, h Host, o UninstallOptions) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	if o.Purge && !o.Yes {
		return errors.New("--purge deletes Secrets, the cluster CA and volume data under " + dirState + "; add --yes to confirm")
	}
	rec, found, err := loadRecord(ctx, h)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no install record at %s: init did not run here, or the record was removed; nothing to uninstall", pathRecord)
	}
	f, err := h.Facts(ctx)
	if err != nil {
		return err
	}
	say := func(tag, what, detail string) {
		fmt.Fprintf(out, "%-6s %s: %s\n", tag, what, detail)
	}

	removeRuntime := rec.ContainerdOwned
	if rec.ContainerdOwned && f.ContainerdSocket {
		busy, err := containerdBusy(ctx, h)
		switch {
		case err != nil:
			removeRuntime = false
			say("keep", "containerd", "could not list its containers ("+err.Error()+"); containerd is left in place")
		case len(busy) > 0:
			removeRuntime = false
			say("keep", "containerd", "containers outside namespace shoal are running in: "+strings.Join(busy, ", ")+"; containerd is left in place")
		}
	}

	// Stop and disable the units first.
	var units []string
	for _, u := range []string{unitAgent, unitServer} {
		units = append(units, u)
	}
	if removeRuntime {
		units = append(units, unitContainerd)
	}
	for _, u := range units {
		p := "/etc/systemd/system/" + u
		if !h.Exists(p) {
			continue
		}
		if o.DryRun {
			say("WOULD", u, "stop and disable")
			continue
		}
		if _, err := h.Run(ctx, "systemctl", "disable", "--now", u); err != nil {
			return fmt.Errorf("stop %s: %w", u, err)
		}
	}

	// Files: the fixed shoal paths, then what init created (runtime paths only when removing containerd).
	fixed := []string{pathShoalBin, pathServerUnit, pathAgentUnit, pathSysctl, pathModules}
	var remove []string
	var kept []string
	for _, p := range append(fixed, rec.Owned...) {
		if contains(remove, p) || contains(kept, p) {
			continue
		}
		if isRuntimePath(p) && !removeRuntime {
			kept = append(kept, p)
			continue
		}
		remove = append(remove, p)
	}
	unitRemoved := false
	for _, p := range remove {
		if !h.Exists(p) {
			continue
		}
		if strings.HasSuffix(p, ".service") {
			unitRemoved = true
		}
		if o.DryRun {
			say("WOULD", "remove", p)
			continue
		}
		if err := h.Remove(p); err != nil {
			return err
		}
		say("done", "remove", p)
	}
	if unitRemoved && !o.DryRun {
		if _, err := h.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if h.Exists(dirRun) {
		if o.DryRun {
			say("WOULD", "remove", dirRun)
		} else if err := h.RemoveAll(dirRun); err != nil {
			return err
		} else {
			say("done", "remove", dirRun)
		}
	}

	if o.Purge {
		if o.DryRun {
			say("WOULD", "remove", dirState+" (cluster CA, Secrets, volumes, install record)")
		} else {
			if err := h.RemoveAll(dirState); err != nil {
				return err
			}
			say("done", "remove", dirState+" (cluster CA, Secrets, volumes, install record)")
		}
	} else {
		say("keep", dirState, "kept: cluster CA, secret key, state and volumes (use --purge --yes to delete them)")
		if !o.DryRun {
			rec.Owned = kept
			if removeRuntime {
				rec.ContainerdOwned = false
			}
			if err := saveRecord(h, rec); err != nil {
				return err
			}
		}
	}
	if o.UserHome != "" {
		say("keep", path.Join(o.UserHome, ".shoal", "config"), "kept: your admin config; delete it by hand if you no longer need it")
	}
	if rec.SwapFile {
		say("keep", "/swapfile", "swap from --swap is kept; remove it by hand with swapoff /swapfile if you want it gone")
	}
	return nil
}

// isRuntimePath is true for containerd, runc and CNI files.
func isRuntimePath(p string) bool {
	return p == pathRunc || p == pathContainerd || p == pathContainerdConfig || p == pathContainerdUnit ||
		p == dirBin+"/ctr" || strings.HasPrefix(p, dirBin+"/containerd") || strings.HasPrefix(p, dirCNI+"/")
}

// containerdBusy lists the namespaces other than shoal that still hold containers.
func containerdBusy(ctx context.Context, h Host) ([]string, error) {
	nsOut, err := h.Run(ctx, "ctr", "--address", containerdSocket, "namespaces", "list", "-q")
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, ns := range strings.Fields(nsOut) {
		if ns == "shoal" {
			continue
		}
		c, err := h.Run(ctx, "ctr", "--address", containerdSocket, "-n", ns, "containers", "list", "-q")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(c) != "" {
			busy = append(busy, ns)
		}
	}
	return busy, nil
}
