// Package runtime defines the container runtime abstraction used by the agent.
//
// Implementations must be idempotent where documented so that the agent's
// reconciler can safely retry any operation.
package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned when a container does not exist.
	ErrNotFound = errors.New("container not found")
	// ErrAlreadyExists is returned by Create when the container ID is taken.
	ErrAlreadyExists = errors.New("container already exists")
)

// PortMapping publishes a container port on the node.
type PortMapping struct {
	ContainerPort int32  `json:"containerPort"`
	HostPort      int32  `json:"hostPort,omitempty"` // 0 = not published on the node
	Protocol      string `json:"protocol,omitempty"` // "tcp" (default) or "udp"
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	ID          string
	Image       string
	Command     []string // replaces the image ENTRYPOINT+CMD when non-empty
	Env         []string // KEY=VALUE, applied on top of the image environment
	Ports       []PortMapping
	CPUMillis   int64 // hard limit (CFS quota); 0 = unlimited
	MemoryBytes int64 // hard limit; 0 = unlimited
	Labels      map[string]string
	// Mounts are read-only bind mounts of host directories. (omitempty: the
	// agent hashes this struct, and containers from before mounts existed
	// must keep their hash.)
	Mounts []Mount `json:",omitempty"`
	// AllowPrivilegeEscalation clears the process's no_new_privs flag, which
	// containerd sets by default. Only the build daemon sets it: RootlessKit
	// needs setuid-style helpers to map its user namespace (ADR 0021). The zero
	// value keeps the default for every workload.
	AllowPrivilegeEscalation bool `json:",omitempty"`
}

// NetNSer is implemented by a runtime that can name a container's network
// namespace. The builder firewalls its daemon's namespace through it (ADR 0021).
type NetNSer interface {
	NetNSPath(ctx context.Context, id string) (string, error)
}

// Mount bind-mounts a host directory into the container. It is read-only
// unless ReadWrite is set. The zero value is read-only on purpose: spec
// hashes cover mounts, and Secret and Config mounts must keep their hashes
// (and so must not restart containers) after an upgrade (ADR 0016, 1.3).
type Mount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"readWrite,omitempty"`
}

// State is the coarse lifecycle state of a container.
type State string

const (
	StateCreated State = "created" // exists but has never been started
	StateRunning State = "running"
	StateExited  State = "exited"
	StateUnknown State = "unknown"
)

// ContainerStatus is the observed state of a container.
type ContainerStatus struct {
	ID         string
	Image      string
	Labels     map[string]string
	State      State
	ExitCode   int // valid when State == StateExited; -1 if unknown (e.g. after host reboot)
	StartedAt  time.Time
	FinishedAt time.Time
	Starts     int // number of times the container has been started
	IP         string
	// StopRequested is set when the container was stopped through Stop
	// rather than exiting on its own. Cleared by the next Start.
	StopRequested bool
}

// LogOptions controls log retrieval.
type LogOptions struct {
	Follow bool
	Tail   int // number of most recent lines; negative = all
}

// LogEntry is a single line of container output.
type LogEntry struct {
	Time   time.Time
	Stream string // "stdout" or "stderr"
	Line   string
}

// ExecOutputLimit caps each of stdout and stderr of an Exec call, in bytes.
// Output beyond it is dropped and ExecResult.Truncated is set, so a chatty
// probe cannot grow the agent's memory.
const ExecOutputLimit = 16 << 10

// ExecResult is the outcome of Exec.
type ExecResult struct {
	ExitCode  int // -1 when the command was killed at its timeout
	Stdout    string
	Stderr    string
	Truncated bool // one of the streams hit ExecOutputLimit
}

// CappedBuffer is an io.Writer that keeps at most Limit bytes and drops the
// rest. It never returns an error, so a full buffer does not stop the command
// writing to it. It is safe for concurrent use: a stream copier may still be
// writing when the caller reads it.
type CappedBuffer struct {
	Limit int

	mu        sync.Mutex
	buf       []byte
	truncated bool
}

// Write keeps up to Limit bytes of p.
func (c *CappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.Limit - len(c.buf)
	if room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	if len(p) > room {
		c.truncated = true
	}
	return len(p), nil
}

// String returns the kept bytes.
func (c *CappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// Truncated reports whether bytes were dropped.
func (c *CappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// Runtime manages containers on a single node.
type Runtime interface {
	// EnsureImage makes the image available locally, pulling it if missing.
	// auth holds the logins the pull may use; nil pulls anonymously.
	EnsureImage(ctx context.Context, ref string, auth []RegistryAuth) error
	// Create creates a container without starting it. The image must already
	// be present. Returns ErrAlreadyExists if the ID is taken.
	Create(ctx context.Context, spec ContainerSpec) error
	// Start starts (or restarts) the container's process. It is a no-op if
	// the container is already running.
	Start(ctx context.Context, id string) error
	// Stop sends SIGTERM and, after timeout, SIGKILL. No-op if not running.
	// The exit status and StopRequested are retained until the next Start or
	// Remove.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Remove force-removes the container and its resources. No-op if absent.
	Remove(ctx context.Context, id string) error
	// Status returns the container's state, or ErrNotFound.
	Status(ctx context.Context, id string) (ContainerStatus, error)
	// List returns all containers managed by this runtime.
	List(ctx context.Context) ([]ContainerStatus, error)
	// Logs calls fn for each log line. With Follow it keeps streaming until
	// the container's process exits, ctx is cancelled or fn returns an error.
	Logs(ctx context.Context, id string, opts LogOptions, fn func(LogEntry) error) error
	// Exec runs argv inside the running container id, without a shell, and
	// waits for it to exit. The command is killed when timeout passes (its
	// ExitCode is then -1 and the error says so). Output is capped at
	// ExecOutputLimit per stream. It returns ErrNotFound for an unknown
	// container and an error if the container is not running.
	// opts may add environment entries (WithEnv) for the process.
	Exec(ctx context.Context, id string, argv []string, timeout time.Duration, opts ...ExecOption) (ExecResult, error)
}

// ExecOption changes one Exec call.
type ExecOption func(*execOptions)

type execOptions struct{ env []string }

// WithEnv adds KEY=VALUE entries to the environment of the exec process, on
// top of the container's own. Put secrets here, not in argv: argv is visible
// in the process list.
func WithEnv(kv ...string) ExecOption {
	return func(o *execOptions) { o.env = append(o.env, kv...) }
}

// ExecEnv returns the entries that opts add to an exec process environment.
func ExecEnv(opts ...ExecOption) []string {
	var o execOptions
	for _, f := range opts {
		f(&o)
	}
	return o.env
}
