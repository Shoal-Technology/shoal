package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/kaiserern/shoal/internal/resource"
	"github.com/kaiserern/shoal/internal/runtime"
)

// standaloneFile is the on-disk format for `shoal agent --standalone -f`.
type standaloneFile struct {
	Replicas []standaloneReplica `json:"replicas"`
}

type standaloneReplica struct {
	ID        string                `json:"id"`
	Workload  string                `json:"workload,omitempty"`
	Image     string                `json:"image"`
	Command   []string              `json:"command,omitempty"`
	Env       map[string]string     `json:"env,omitempty"`
	Ports     []runtime.PortMapping `json:"ports,omitempty"`
	Resources struct {
		CPU    string `json:"cpu,omitempty"`
		Memory string `json:"memory,omitempty"`
	} `json:"resources,omitempty"`
	RestartPolicy RestartPolicy `json:"restartPolicy,omitempty"`
	Desired       DesiredState  `json:"desired,omitempty"`
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ParseAssignments decodes and validates a standalone replicas file.
// Unknown fields are rejected so typos fail loudly.
func ParseAssignments(data []byte) ([]Assignment, error) {
	var f standaloneFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("decode replicas file: %w", err)
	}
	seen := map[string]bool{}
	out := make([]Assignment, 0, len(f.Replicas))
	var errs []error
	for i, r := range f.Replicas {
		a, err := r.toAssignment()
		if err != nil {
			errs = append(errs, fmt.Errorf("replicas[%d] (%s): %w", i, r.ID, err))
			continue
		}
		if seen[a.ReplicaID] {
			errs = append(errs, fmt.Errorf("replicas[%d]: duplicate id %q", i, r.ID))
			continue
		}
		seen[a.ReplicaID] = true
		out = append(out, a)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func (r standaloneReplica) toAssignment() (Assignment, error) {
	if len(r.ID) > 63 || !dnsLabel.MatchString(r.ID) {
		return Assignment{}, fmt.Errorf("id must be a lowercase DNS label (a-z, 0-9, '-', max 63 chars)")
	}
	if r.Image == "" {
		return Assignment{}, fmt.Errorf("image is required")
	}
	a := Assignment{
		ReplicaID:     r.ID,
		Workload:      r.Workload,
		RestartPolicy: r.RestartPolicy,
		Desired:       r.Desired,
		Spec:          runtime.ContainerSpec{Image: r.Image, Command: r.Command, Ports: r.Ports},
	}
	if a.Workload == "" {
		a.Workload = r.ID
	}
	switch a.RestartPolicy {
	case "":
		a.RestartPolicy = RestartAlways
	case RestartAlways, RestartOnFailure, RestartNever:
	default:
		return Assignment{}, fmt.Errorf("restartPolicy must be Always, OnFailure or Never")
	}
	switch a.Desired {
	case "":
		a.Desired = DesiredRunning
	case DesiredRunning, DesiredStopped:
	default:
		return Assignment{}, fmt.Errorf("desired must be Running or Stopped")
	}
	for _, k := range slices.Sorted(maps.Keys(r.Env)) {
		if !envName.MatchString(k) {
			return Assignment{}, fmt.Errorf("invalid env var name %q", k)
		}
		a.Spec.Env = append(a.Spec.Env, k+"="+r.Env[k])
	}
	hostPorts := map[string]bool{}
	for i := range a.Spec.Ports {
		p := &a.Spec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return Assignment{}, fmt.Errorf("ports[%d]: protocol must be tcp or udp", i)
		}
		if p.ContainerPort < 1 || p.ContainerPort > 65535 || p.HostPort < 0 || p.HostPort > 65535 {
			return Assignment{}, fmt.Errorf("ports[%d]: port out of range", i)
		}
		key := fmt.Sprintf("%d/%s", p.HostPort, p.Protocol)
		if p.HostPort != 0 && hostPorts[key] {
			return Assignment{}, fmt.Errorf("ports[%d]: duplicate hostPort %s", i, key)
		}
		hostPorts[key] = true
	}
	var err error
	if r.Resources.CPU != "" {
		if a.Spec.CPUMillis, err = resource.ParseCPU(r.Resources.CPU); err != nil {
			return Assignment{}, err
		}
	}
	if r.Resources.Memory != "" {
		if a.Spec.MemoryBytes, err = resource.ParseMemory(r.Resources.Memory); err != nil {
			return Assignment{}, err
		}
	}
	return a, nil
}

// Standalone runs the reconciler against a local replicas file instead of a
// server. The file is re-read whenever it changes. If it is missing or
// invalid, the last valid desired state is kept (containers are never
// removed because of a bad edit).
type Standalone struct {
	Path       string
	Interval   time.Duration
	Reconciler *Reconciler
	Log        *slog.Logger
}

// Run reconciles until ctx is cancelled. Containers keep running after it
// returns, so the agent can be restarted without disrupting workloads.
func (s *Standalone) Run(ctx context.Context) error {
	var (
		desired []Assignment
		loaded  bool
		modTime time.Time
		prev    = map[string]ReplicaStatus{}
	)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if fi, err := os.Stat(s.Path); err != nil {
			s.Log.Error("cannot read replicas file, keeping previous desired state", "path", s.Path, "err", err)
		} else if !loaded || !fi.ModTime().Equal(modTime) {
			modTime = fi.ModTime()
			if as, err := loadAssignments(s.Path); err != nil {
				s.Log.Error("invalid replicas file, keeping previous desired state", "path", s.Path, "err", err)
			} else {
				desired, loaded = as, true
				s.Log.Info("loaded desired state", "path", s.Path, "replicas", len(desired))
			}
		}

		if loaded {
			statuses, err := s.Reconciler.Reconcile(ctx, desired)
			if err != nil && ctx.Err() == nil {
				s.Log.Warn("reconcile incomplete", "err", err)
			}
			prev = logTransitions(s.Log, prev, statuses)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func loadAssignments(path string) ([]Assignment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read replicas file: %w", err)
	}
	return ParseAssignments(b)
}

// logTransitions logs replicas whose phase, restarts or message changed
// since prev, and returns the new state to compare against next time.
func logTransitions(log *slog.Logger, prev map[string]ReplicaStatus, statuses []ReplicaStatus) map[string]ReplicaStatus {
	next := make(map[string]ReplicaStatus, len(statuses))
	for _, st := range statuses {
		next[st.ReplicaID] = st
		p, ok := prev[st.ReplicaID]
		if ok && p.Phase == st.Phase && p.Restarts == st.Restarts && p.Message == st.Message {
			continue
		}
		attrs := []any{"replica", st.ReplicaID, "phase", st.Phase, "restarts", st.Restarts}
		if st.IP != "" {
			attrs = append(attrs, "ip", st.IP)
		}
		if st.Phase == PhaseFailed || st.Phase == PhaseSucceeded {
			attrs = append(attrs, "exitCode", st.ExitCode)
		}
		if st.Message != "" {
			attrs = append(attrs, "message", st.Message)
		}
		log.Info("replica status", attrs...)
	}
	for id := range prev {
		if _, ok := next[id]; !ok {
			log.Info("replica removed", "replica", id)
		}
	}
	return next
}
