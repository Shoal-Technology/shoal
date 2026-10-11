// Package volume manages the node-local volumes of a Shoal agent (ADR 0016,
// section 1). A volume is a directory the agent bind-mounts into a container.
// Two backends back it: dir, a plain directory with no size limit, and loop,
// an ext4 image of the requested size mounted through a loop device.
//
// Layout under the root (<state-dir>/volumes):
//
//	<name>             the directory bind-mounted into containers
//	<name>.img         loop backend only: the ext4 image
//	<name>.meta.json   retention and backend, written at create
//	.retained/         data of deleted volumes with Retention Retain
package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Backends.
const (
	BackendLoop = "loop"
	BackendDir  = "dir"
)

// Retention values (the API's spec.retention).
const (
	RetentionRetain = "Retain"
	RetentionDelete = "Delete"
)

const (
	retainedDir = ".retained"
	metaSuffix  = ".meta.json"
	imageSuffix = ".img"
)

// Spec is what the agent needs to create a volume.
type Spec struct {
	Name      string
	Bytes     int64 // size; enforced by the loop backend only
	UID, GID  int
	Mode      os.FileMode
	Retention string
}

// Manager creates, mounts and retires the volumes of one node.
type Manager interface {
	// Backend returns BackendLoop or BackendDir.
	Backend() string
	// Ensure makes the volume exist and mounted, and returns the host
	// directory to bind-mount. Owner and mode are applied only when the
	// volume is created, because they are fixed at create (ADR 0016, 1.7).
	// It is idempotent and re-mounts an existing loop volume after a restart.
	Ensure(ctx context.Context, s Spec) (string, error)
	// Retire removes a volume that is no longer assigned to this node. Its
	// data moves to .retained for Retain, and is removed for Delete. The
	// retention is the one recorded at Ensure.
	Retire(ctx context.Context, name string) error
	// Known returns the names of the volumes on disk.
	Known() ([]string, error)
}

// Runner runs a system tool and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
	}
	return s, nil
}

// meta is the per-volume file written at create.
type meta struct {
	Name      string `json:"name"`
	Retention string `json:"retention"`
	Backend   string `json:"backend"`
}

// base holds what both backends share. The functions are fields so tests can
// replace them.
type base struct {
	root    string
	backend string
	log     *slog.Logger
	now     func() time.Time
	chown   func(path string, uid, gid int) error
}

// newBase returns the shared state for a root directory.
func newBase(root, backend string, log *slog.Logger) base {
	if log == nil {
		log = slog.Default()
	}
	return base{
		root: root, backend: backend, log: log, now: time.Now,
		chown: func(path string, uid, gid int) error { return os.Chown(path, uid, gid) },
	}
}

func (b *base) Backend() string { return b.backend }

// dirPath is the host directory of a volume.
func (b *base) dirPath(name string) string { return filepath.Join(b.root, name) }

// validName is a volume name that is safe as a file name. The API allows
// only lowercase DNS labels; this is a second check at the file system.
var validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func checkName(name string) error {
	if !validName.MatchString(name) || len(name) > 63 {
		return fmt.Errorf("volume name %q is not a DNS label", name)
	}
	return nil
}

// writeMeta records the volume's retention if it is not recorded already.
// Writes go through a temporary file, so a crash never leaves a partial file.
func (b *base) writeMeta(name, retention, backend string) error {
	want := meta{Name: name, Retention: retention, Backend: backend}
	path := filepath.Join(b.root, name+metaSuffix)
	if old, err := b.readMeta(name); err == nil && old == want {
		return nil
	}
	data, err := json.Marshal(want)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (b *base) readMeta(name string) (meta, error) {
	var m meta
	data, err := os.ReadFile(filepath.Join(b.root, name+metaSuffix))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// retention returns the recorded retention, or Retain when there is no
// record: keeping data is the safe default.
func (b *base) retention(name string) string {
	m, err := b.readMeta(name)
	if err != nil || m.Retention == "" {
		return RetentionRetain
	}
	return m.Retention
}

// Known lists the volumes that have a meta file under the root.
func (b *base) Known() ([]string, error) {
	entries, err := os.ReadDir(b.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), metaSuffix); ok && !e.IsDir() {
			out = append(out, name)
		}
	}
	return out, nil
}

// retainedName is where a retired volume's data goes.
func (b *base) retainedName(name string) (string, error) {
	dir := filepath.Join(b.root, retainedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d", name, b.now().UnixNano())), nil
}

// moveMeta moves the meta file of a retired volume next to its data, or
// removes it, so Known no longer lists the volume.
func (b *base) moveMeta(name, dst string) error {
	src := filepath.Join(b.root, name+metaSuffix)
	if dst == "" {
		err := os.Remove(src)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Rename(src, dst+metaSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Purge removes the retained data of name, as `shoal volume purge` does. It
// matches only <name>-<digits> entries, so purging pg never touches pg-data.
// It returns the removed entries.
func Purge(root, name string) ([]string, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, retainedDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	match := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[0-9]+(` + regexp.QuoteMeta(imageSuffix) + `|` + regexp.QuoteMeta(metaSuffix) + `)?$`)
	var removed []string
	var errs []error
	for _, e := range entries {
		if !match.MatchString(e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed, errors.Join(errs...)
}

// NewManager returns the manager for backend under root. When loop is asked
// for but the host cannot run it, dir is used and a warning is logged; the
// caller reports the backend actually in use.
func NewManager(root, backend string, log *slog.Logger) (Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	switch backend {
	case BackendLoop:
		if missing := loopMissing(); len(missing) > 0 {
			log.Warn("loop devices are not usable on this node; volumes use the dir backend, which has no size limit",
				"missing", strings.Join(missing, ", "))
			return newDir(root, log), nil
		}
		return newLoop(root, log, execRunner{}), nil
	case BackendDir:
		return newDir(root, log), nil
	}
	return nil, fmt.Errorf("unknown volume backend %q (want loop or dir)", backend)
}
