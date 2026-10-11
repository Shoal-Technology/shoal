// Package runtime defines the container runtime abstraction used by the agent.
//
// Implementations must be idempotent where documented so that the agent's
// reconciler can safely retry any operation.
package runtime

import (
	"context"
	"errors"
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

// Runtime manages containers on a single node.
type Runtime interface {
	// EnsureImage makes the image available locally, pulling it if missing.
	EnsureImage(ctx context.Context, ref string) error
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
}
