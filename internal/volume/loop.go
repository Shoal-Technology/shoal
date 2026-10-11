package volume

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// loopManager keeps each volume as an ext4 image of the requested size. The
// image is attached to a loop device and mounted at the volume's directory.
// The size is enforced by ext4. Growing a volume is offline work (ADR 0016,
// section 1.2). The system tools are run through a Runner, so no new Go
// dependency is needed; the code only builds the command lines.
type loopManager struct {
	base
	run       Runner
	mountinfo func() ([]byte, error)
}

func newLoop(root string, log *slog.Logger, run Runner) *loopManager {
	return &loopManager{
		base:      newBase(root, BackendLoop, log),
		run:       run,
		mountinfo: func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") },
	}
}

// loopMissing lists what the loop backend needs that this host lacks.
func loopMissing() []string {
	var missing []string
	for _, tool := range []string{"losetup", "mount", "umount", "mkfs.ext4"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		missing = append(missing, "/dev/loop-control")
	}
	return missing
}

func (m *loopManager) image(name string) string {
	return filepath.Join(m.root, name+imageSuffix)
}

func (m *loopManager) Ensure(ctx context.Context, s Spec) (string, error) {
	if err := checkName(s.Name); err != nil {
		return "", err
	}
	if s.Bytes <= 0 {
		return "", fmt.Errorf("volume %s: size must be set for the loop backend", s.Name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return "", err
	}
	img, mnt := m.image(s.Name), m.dirPath(s.Name)
	created := false
	_, err := os.Stat(img)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := createSparse(img, s.Bytes); err != nil {
			return "", err
		}
		if _, err := m.run.Run(ctx, "mkfs.ext4", "-q", "-F", img); err != nil {
			_ = os.Remove(img) // a half-formatted image must not be reused
			return "", fmt.Errorf("format volume %s: %w", s.Name, err)
		}
		created = true
	case err != nil:
		return "", err
	}
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return "", err
	}
	mounted, err := m.isMounted(mnt)
	if err != nil {
		return "", err
	}
	if !mounted {
		if err := m.attachAndMount(ctx, img, mnt); err != nil {
			return "", fmt.Errorf("mount volume %s: %w", s.Name, err)
		}
	}
	// Owner and mode are applied once, at the first successful mount.
	if _, metaErr := m.readMeta(s.Name); created || errors.Is(metaErr, fs.ErrNotExist) {
		if err := m.chown(mnt, s.UID, s.GID); err != nil {
			return "", fmt.Errorf("chown volume %s: %w", s.Name, err)
		}
		if err := os.Chmod(mnt, s.Mode); err != nil {
			return "", fmt.Errorf("chmod volume %s: %w", s.Name, err)
		}
	}
	if err := m.writeMeta(s.Name, s.Retention, BackendLoop); err != nil {
		return "", err
	}
	return mnt, nil
}

// attachAndMount attaches img to a free loop device and mounts it at mnt. The
// mount options keep setuid, device nodes and execution working in the
// image's own files; the container's bind mount does not add noexec.
func (m *loopManager) attachAndMount(ctx context.Context, img, mnt string) error {
	out, err := m.run.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		return err
	}
	dev := strings.TrimSpace(out)
	if !strings.HasPrefix(dev, "/dev/loop") {
		return fmt.Errorf("losetup returned %q, not a loop device", dev)
	}
	if _, err := m.run.Run(ctx, "mount", "-o", "nosuid,nodev", dev, mnt); err != nil {
		// Detach, so the device is not leaked. The caller's context may be
		// done, so this uses a fresh one.
		_, _ = m.run.Run(context.WithoutCancel(ctx), "losetup", "-d", dev)
		return err
	}
	return nil
}

// isMounted reports whether dir is a mount point, from /proc/self/mountinfo.
func (m *loopManager) isMounted(dir string) (bool, error) {
	data, err := m.mountinfo()
	if err != nil {
		return false, err
	}
	return mountinfoHas(data, dir), nil
}

// mountinfoHas reports whether the mountinfo text lists dir as a mount point.
// The mount point is field 5; spaces and a few other bytes are octal-escaped.
func mountinfoHas(data []byte, dir string) bool {
	want := filepath.Clean(dir)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMount(fields[4]) == want {
			return true
		}
	}
	return false
}

// unescapeMount decodes the \ooo escapes of a mountinfo path.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// attachedDevices lists the loop devices that use img.
func (m *loopManager) attachedDevices(ctx context.Context, img string) ([]string, error) {
	out, err := m.run.Run(ctx, "losetup", "-j", img)
	if err != nil {
		return nil, err
	}
	var devs []string
	for _, line := range strings.Split(out, "\n") {
		// Lines look like "/dev/loop0: [2049]:1234 (/path/img)".
		dev, _, ok := strings.Cut(line, ":")
		if ok && strings.HasPrefix(dev, "/dev/loop") {
			devs = append(devs, dev)
		}
	}
	return devs, nil
}

func (m *loopManager) Retire(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	img, mnt := m.image(name), m.dirPath(name)
	if mounted, err := m.isMounted(mnt); err != nil {
		return err
	} else if mounted {
		if _, err := m.run.Run(ctx, "umount", mnt); err != nil {
			return fmt.Errorf("unmount volume %s: %w", name, err)
		}
	}
	if _, err := os.Stat(img); errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(mnt)
		return m.moveMeta(name, "")
	}
	devs, err := m.attachedDevices(ctx, img)
	if err != nil {
		return err
	}
	for _, dev := range devs {
		if _, err := m.run.Run(ctx, "losetup", "-d", dev); err != nil {
			return fmt.Errorf("detach %s: %w", dev, err)
		}
	}
	if m.retention(name) == RetentionDelete {
		if err := os.Remove(img); err != nil {
			return err
		}
		_ = os.Remove(mnt)
		return m.moveMeta(name, "")
	}
	dst, err := m.retainedName(name)
	if err != nil {
		return err
	}
	if err := os.Rename(img, dst+imageSuffix); err != nil {
		return fmt.Errorf("retain volume %s: %w", name, err)
	}
	_ = os.Remove(mnt)
	m.log.Info("volume data retained", "volume", name, "path", filepath.Base(dst+imageSuffix))
	return m.moveMeta(name, dst)
}

// createSparse creates img with the given size, failing if it exists.
func createSparse(img string, size int64) error {
	f, err := os.OpenFile(img, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		_ = os.Remove(img)
		return err
	}
	return f.Close()
}
