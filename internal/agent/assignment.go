// Package agent runs on every worker node and drives the local container
// runtime toward the set of replicas assigned to the node.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/volume"
)

// DesiredState is what the agent should do with an assigned replica.
type DesiredState string

const (
	DesiredRunning DesiredState = "Running"
	DesiredStopped DesiredState = "Stopped"
)

// RestartPolicy controls whether exited containers are restarted.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "Always"
	RestartOnFailure RestartPolicy = "OnFailure"
	RestartNever     RestartPolicy = "Never"
)

// Assignment is one replica the node should run. Spec.ID is ignored;
// ReplicaID is used as the container ID.
type Assignment struct {
	ReplicaID     string
	Workload      string
	Spec          runtime.ContainerSpec
	RestartPolicy RestartPolicy
	Desired       DesiredState
	// Health is probed while the container runs; nil = running is ready.
	// It is not part of the container, so changing it needs no recreate.
	Health *HealthCheck
	// TemplateGeneration is reported back with the status, so the server
	// knows which template the status describes.
	TemplateGeneration int64
	// Files are written to host directories and bind-mounted read-only;
	// their content may change without recreating the container.
	Files []FileMount
	// EnvBasis replaces Spec.Env in the spec hash when env values come
	// from Secrets or Configs: the references (not the values) decide
	// whether the container must be recreated, so a changed Secret doesn't
	// restart every replica at once.
	EnvBasis string
	// Problem, if set, is why the replica can't run as specified (e.g. a
	// missing Secret). It is reported, and an existing container is left
	// alone.
	Problem string
	// RestartedAt is the template's restartedAt: a new value recreates the
	// container (a restart).
	RestartedAt string
	// Volumes are the node-local volumes the replica mounts (ADR 0016). Their
	// host directories are added to Spec.Mounts when the pass runs.
	Volumes []VolumeAttachment
	// Job is the backup or restore to run in the replica (ADR 0016, pass D).
	Job *v1alpha1.DatabaseJob
}

// VolumeAttachment is a Volume the replica mounts, with what the agent needs
// to create it.
type VolumeAttachment struct {
	Name        string
	Destination string // in the container
	ReadWrite   bool
	Spec        volume.Spec
}

// bindVolumes resolves the template's volume mounts against the Volumes that
// the server bound to this node. A missing Volume is the assignment's problem,
// as a missing Secret is: the replica is reported and left alone.
func bindVolumes(a *Assignment, t *v1alpha1.WorkloadSpec, vols map[string]*v1alpha1.Volume) error {
	for _, m := range t.GetVolumes() {
		v, ok := vols[m.GetName()]
		if !ok {
			if a.Problem == "" {
				a.Problem = fmt.Sprintf("volume %q is not on this node", m.GetName())
			}
			continue
		}
		spec, err := volumeSpec(v)
		if err != nil {
			return fmt.Errorf("volume %s: %w", m.GetName(), err)
		}
		a.Volumes = append(a.Volumes, VolumeAttachment{
			Name: m.GetName(), Destination: m.GetMountPath(), ReadWrite: !m.GetReadOnly(), Spec: spec,
		})
	}
	return nil
}

// snapshotVolumeNames lists the Volumes the server bound to this node.
func snapshotVolumeNames(snap *v1alpha1.AssignmentSnapshot) []string {
	out := make([]string, 0, len(snap.GetVolumes()))
	for _, v := range snap.GetVolumes() {
		out = append(out, v.GetMetadata().GetName())
	}
	return out
}

// volumeSpec converts a Volume from the API into what the volume manager
// needs.
func volumeSpec(v *v1alpha1.Volume) (volume.Spec, error) {
	size, err := resource.VolumeBytes(v.GetSpec())
	if err != nil {
		return volume.Spec{}, err
	}
	mode, err := strconv.ParseUint(v.GetSpec().GetMode(), 8, 32)
	if err != nil {
		return volume.Spec{}, fmt.Errorf("mode %q: %w", v.GetSpec().GetMode(), err)
	}
	owner := v.GetSpec().GetOwner()
	return volume.Spec{
		Name: v.GetMetadata().GetName(), Bytes: size,
		UID: int(owner.GetUid()), GID: int(owner.GetGid()),
		Mode: os.FileMode(mode), Retention: v.GetSpec().GetRetention(),
	}, nil
}

// FileMount is a directory of files mounted into the container.
type FileMount struct {
	Path  string            // in the container
	Files map[string]string // name -> content
}

// Container labels set by the agent.
const (
	LabelReplicaID   = "shoal.replica-id"
	LabelWorkload    = "shoal.workload"
	LabelSpecHash    = "shoal.spec-hash"
	LabelTemplateGen = "shoal.template-generation" // generation the container was created for
)

// containerSpec returns the runtime spec with agent labels applied.
func (a Assignment) containerSpec() runtime.ContainerSpec {
	s := a.Spec
	s.ID = a.ReplicaID
	s.Labels = maps.Clone(a.Spec.Labels)
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[LabelReplicaID] = a.ReplicaID
	s.Labels[LabelWorkload] = a.Workload
	s.Labels[LabelSpecHash] = a.specHash()
	s.Labels[LabelTemplateGen] = strconv.FormatInt(a.TemplateGeneration, 10)
	return s
}

// specHash identifies the container-relevant part of the assignment. A
// changed hash means the container must be recreated. Restart policy and
// desired state are excluded: they don't require a new container.
func (a Assignment) specHash() string {
	s := a.Spec
	s.ID, s.Labels = "", nil
	if a.EnvBasis != "" {
		s.Env = []string{a.EnvBasis}
	}
	b, _ := json.Marshal(s) // plain struct of strings/ints: cannot fail
	if a.RestartedAt != "" {
		b = append(b, "restarted:"+a.RestartedAt...) // only when set: other hashes stay as they were
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Phase summarises a replica's state for reporting.
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
	PhaseStopped   Phase = "Stopped"
)

// ReplicaStatus is the observed state of an assigned replica.
type ReplicaStatus struct {
	ReplicaID string
	Phase     Phase
	Restarts  int
	ExitCode  int
	IP        string
	StartedAt time.Time
	Message   string
	// Ready: running and passing the health check (if any).
	Ready              bool
	TemplateGeneration int64
	// Job is the result of the replica's database job, once it has run
	// (ADR 0016, pass D). Nil while it runs or when there is none.
	Job *v1alpha1.DatabaseJobResult
	// outdated: the running container predates the assignment's spec (it
	// is kept while the new image is pulled).
	outdated bool
}

// API converts the status for reporting to the server.
func (s ReplicaStatus) API() *v1alpha1.ReplicaStatus {
	st := &v1alpha1.ReplicaStatus{
		Phase:              string(s.Phase),
		Restarts:           int32(s.Restarts),
		ExitCode:           int32(s.ExitCode),
		Ip:                 s.IP,
		Message:            s.Message,
		Ready:              s.Ready,
		TemplateGeneration: s.TemplateGeneration,
		Job:                s.Job,
	}
	if !s.StartedAt.IsZero() {
		st.StartedAt = timestamppb.New(s.StartedAt)
	}
	return st
}

// AssignmentFromAPI converts an assignment received from the server.
func AssignmentFromAPI(a *v1alpha1.Assignment) (Assignment, error) {
	t := a.GetTemplate()
	out := Assignment{
		ReplicaID:     a.GetReplica(),
		Workload:      a.GetWorkload(),
		RestartPolicy: RestartPolicy(t.GetRestartPolicy()),
		Desired:       DesiredRunning,
		Spec:          runtime.ContainerSpec{Image: t.GetImage(), Command: t.GetCommand()},

		TemplateGeneration: a.GetTemplateGeneration(),
		Job:                a.GetJob(),
	}
	if out.RestartPolicy == "" {
		out.RestartPolicy = RestartAlways
	}
	if hc := t.GetHealthCheck(); hc != nil {
		h := &HealthCheck{
			Interval:         time.Duration(hc.GetIntervalSeconds()) * time.Second,
			Timeout:          time.Duration(hc.GetTimeoutSeconds()) * time.Second,
			FailureThreshold: int(hc.GetFailureThreshold()),
			InitialDelay:     time.Duration(hc.GetInitialDelaySeconds()) * time.Second,
		}
		switch {
		case hc.GetExec() != nil:
			argv, _ := json.Marshal(hc.GetExec().GetCommand()) // plain strings: cannot fail
			h.Exec = string(argv)
		case hc.GetHttp() != nil:
			h.HTTPPath, h.Port = hc.GetHttp().GetPath(), int(hc.GetHttp().GetPort())
		default:
			h.Port = int(hc.GetTcp().GetPort())
		}
		// Defaults are applied by the server; guard against zero values
		// from older servers or hand-written snapshots.
		h.Interval = max(h.Interval, time.Second)
		h.Timeout = max(h.Timeout, time.Second)
		h.FailureThreshold = max(h.FailureThreshold, 1)
		out.Health = h
	}
	// Env comes resolved from the server (Secret and Config references
	// replaced by their values); the template has the references.
	env := a.GetEnv()
	if len(env) == 0 {
		env = t.GetEnv()
	}
	for _, e := range env {
		out.Spec.Env = append(out.Spec.Env, e.GetName()+"="+e.GetValue())
	}
	for _, e := range t.GetEnv() {
		if e.GetValueFrom() != nil {
			b, _ := json.Marshal(t.GetEnv()) // plain message: cannot fail
			out.EnvBasis = "refs:" + string(b)
			break
		}
	}
	for _, m := range a.GetMounts() {
		out.Files = append(out.Files, FileMount{Path: m.GetPath(), Files: m.GetFiles()})
	}
	out.Problem = a.GetProblem()
	if r := t.GetRestartedAt(); r != nil {
		out.RestartedAt = r.AsTime().UTC().Format(time.RFC3339Nano)
	}
	for _, p := range t.GetPorts() {
		out.Spec.Ports = append(out.Spec.Ports, runtime.PortMapping{
			ContainerPort: p.GetContainerPort(), HostPort: p.GetHostPort(), Protocol: p.GetProtocol(),
		})
	}
	var err error
	if s := t.GetResources().GetCpu(); s != "" {
		if out.Spec.CPUMillis, err = resource.ParseCPU(s); err != nil {
			return Assignment{}, fmt.Errorf("replica %s: %w", a.GetReplica(), err)
		}
	}
	if s := t.GetResources().GetMemory(); s != "" {
		if out.Spec.MemoryBytes, err = resource.ParseMemory(s); err != nil {
			return Assignment{}, fmt.Errorf("replica %s: %w", a.GetReplica(), err)
		}
	}
	return out, nil
}
