package resource

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	"google.golang.org/protobuf/proto"

	"example.com/shoal/api/v1alpha1"
)

// MaxWorkloadNameLen leaves room for the replica suffix in container IDs and
// host names, which must stay within a 63-character DNS label.
const MaxWorkloadNameLen = 52

// Replica limits keep a typo (replicas: 10000) from flooding a small cluster.
const MaxReplicas = 1000

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidName reports whether name is a lowercase DNS label of at most max
// characters.
func ValidName(name string, max int) bool {
	return len(name) <= max && dnsLabel.MatchString(name)
}

// DefaultWorkload fills in defaults, so stored objects show the effective
// values. It also clears fields managed by the server (metadata other than
// the name and labels, and status), so the output of `get -o yaml` can be re-applied.
func DefaultWorkload(w *v1alpha1.Workload) {
	w.ApiVersion, w.Kind = v1alpha1.APIVersion, v1alpha1.KindWorkload
	w.Metadata = &v1alpha1.ObjectMeta{Name: w.GetMetadata().GetName(), Labels: w.GetMetadata().GetLabels()}
	w.Status = nil
	if w.Spec == nil {
		w.Spec = &v1alpha1.WorkloadSpec{}
	}
	s := w.Spec
	if s.Replicas == nil {
		s.Replicas = proto.Int32(1)
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = "Always"
	}
	for _, p := range s.Ports {
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
	}
	if s.Update == nil {
		s.Update = &v1alpha1.UpdateStrategy{}
	}
	if s.Update.MaxUnavailable == nil {
		s.Update.MaxUnavailable = proto.Int32(1)
	}
	if s.Update.ProgressDeadlineSeconds == 0 {
		s.Update.ProgressDeadlineSeconds = 120
	}
	if hc := s.HealthCheck; hc != nil {
		if hc.IntervalSeconds == 0 {
			hc.IntervalSeconds = 10
		}
		if hc.TimeoutSeconds == 0 {
			hc.TimeoutSeconds = 2
		}
		if hc.FailureThreshold == 0 {
			hc.FailureThreshold = 3
		}
	}
}

// ValidateWorkload checks a defaulted Workload and reports every problem
// found, each prefixed with its field path.
func ValidateWorkload(w *v1alpha1.Workload) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if name := w.GetMetadata().GetName(); name == "" {
		bad("metadata.name", "required")
	} else if !ValidName(name, MaxWorkloadNameLen) {
		bad("metadata.name", "must be a lowercase DNS label (a-z, 0-9, '-') of at most %d characters", MaxWorkloadNameLen)
	}
	s := w.GetSpec()
	if s.GetImage() == "" {
		bad("spec.image", "required")
	} else if _, err := reference.ParseDockerRef(s.GetImage()); err != nil {
		bad("spec.image", "invalid image reference: %v", err)
	}
	if r := s.GetReplicas(); r < 0 || r > MaxReplicas {
		bad("spec.replicas", "must be between 0 and %d", MaxReplicas)
	}

	validateEnvAndFiles(s, bad)
	validateVolumeMounts(s, bad)
	seenEnv := map[string]bool{}
	for i, e := range s.GetEnv() {
		f := fmt.Sprintf("spec.env[%d].name", i)
		switch {
		case !envName.MatchString(e.GetName()):
			bad(f, "invalid environment variable name %q", e.GetName())
		case seenEnv[e.GetName()]:
			bad(f, "duplicate %q", e.GetName())
		}
		seenEnv[e.GetName()] = true
	}

	hostPorts := map[string]bool{}
	for i, p := range s.GetPorts() {
		f := fmt.Sprintf("spec.ports[%d]", i)
		if !validPort(p.GetContainerPort()) {
			bad(f+".containerPort", "must be between 1 and 65535")
		}
		if hp := p.GetHostPort(); hp != 0 {
			key := fmt.Sprintf("%d/%s", hp, p.GetProtocol())
			switch {
			case !validPort(hp):
				bad(f+".hostPort", "must be between 1 and 65535")
			case hostPorts[key]:
				bad(f+".hostPort", "duplicate %s", key)
			}
			hostPorts[key] = true
		}
		if pr := p.GetProtocol(); pr != "tcp" && pr != "udp" {
			bad(f+".protocol", "must be tcp or udp")
		}
	}

	if c := s.GetResources().GetCpu(); c != "" {
		if v, err := ParseCPU(c); err != nil {
			bad("spec.resources.cpu", "%v", err)
		} else if v == 0 {
			bad("spec.resources.cpu", "must be greater than zero (omit it for no limit)")
		}
	}
	if m := s.GetResources().GetMemory(); m != "" {
		if v, err := ParseMemory(m); err != nil {
			bad("spec.resources.memory", "%v", err)
		} else if v < 4<<20 {
			bad("spec.resources.memory", "must be at least 4Mi (omit it for no limit)")
		}
	}

	if hc := s.GetHealthCheck(); hc != nil {
		switch {
		case countSet(hc.Http != nil, hc.Tcp != nil, hc.Exec != nil) != 1:
			bad("spec.healthCheck", "set exactly one of http, tcp or exec")
		case hc.Exec != nil:
			validateExecCommand(hc.Exec.GetCommand(), "spec.healthCheck.exec.command", bad)
		case hc.Http != nil:
			if len(hc.Http.GetPath()) == 0 || hc.Http.GetPath()[0] != '/' {
				bad("spec.healthCheck.http.path", "must start with /")
			}
			if !validPort(hc.Http.GetPort()) {
				bad("spec.healthCheck.http.port", "must be between 1 and 65535")
			}
		default:
			if !validPort(hc.Tcp.GetPort()) {
				bad("spec.healthCheck.tcp.port", "must be between 1 and 65535")
			}
		}
		if hc.IntervalSeconds < 1 || hc.TimeoutSeconds < 1 || hc.FailureThreshold < 1 {
			bad("spec.healthCheck", "intervalSeconds, timeoutSeconds and failureThreshold must be at least 1")
		}
		if hc.TimeoutSeconds > hc.IntervalSeconds {
			bad("spec.healthCheck.timeoutSeconds", "must not exceed intervalSeconds")
		}
		if hc.InitialDelaySeconds < 0 || hc.InitialDelaySeconds > 3600 {
			bad("spec.healthCheck.initialDelaySeconds", "must be between 0 and 3600")
		}
	}

	switch s.GetPartitionPolicy() {
	case "", "Replace", "Hold":
	default:
		bad("spec.partitionPolicy", "must be Replace or Hold")
	}

	switch s.GetRestartPolicy() {
	case "Always", "OnFailure", "Never":
	default:
		bad("spec.restartPolicy", "must be Always, OnFailure or Never")
	}
	// There is no surge: an update replaces replicas in place, so at least
	// one must be allowed to be unavailable.
	if s.GetUpdate().GetMaxUnavailable() < 1 {
		bad("spec.update.maxUnavailable", "must be at least 1")
	}
	if d := s.GetUpdate().GetProgressDeadlineSeconds(); d < 10 || d > 3600 {
		bad("spec.update.progressDeadlineSeconds", "must be between 10 and 3600")
	}
	return errors.Join(errs...)
}

// validateVolumeMounts checks spec.volumes. A read-write volume holds data
// that only one writer may touch, so it needs replicas of at most 1. Whether
// a Volume is referenced by another Workload is checked at apply time, when
// the claim is taken (ADR 0016, section 1.1).
func validateVolumeMounts(s *v1alpha1.WorkloadSpec, bad func(field, format string, args ...any)) {
	fileMountPath := map[string]bool{}
	for _, m := range s.GetFiles() {
		fileMountPath[m.GetPath()] = true
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	for i, m := range s.GetVolumes() {
		f := fmt.Sprintf("spec.volumes[%d]", i)
		n := m.GetName()
		switch {
		case !ValidName(n, 63):
			bad(f+".name", "must be a Volume name (lowercase DNS label of at most 63 characters)")
		case names[n]:
			bad(f+".name", "duplicate volume %q", n)
		}
		names[n] = true
		p := m.GetMountPath()
		switch {
		case !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/":
			bad(f+".mountPath", "must be a clean absolute directory, not /")
		case paths[p] || fileMountPath[p]:
			bad(f+".mountPath", "%s is already used by another mount", p)
		}
		paths[p] = true
		if !m.GetReadOnly() && s.GetReplicas() > 1 {
			bad(f, "a read-write volume needs replicas of at most 1, not %d", s.GetReplicas())
		}
	}
}

func validPort(p int32) bool { return p >= 1 && p <= 65535 }

// countSet counts the true values.
func countSet(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// maxExecArgs bounds an exec probe's command line.
const maxExecArgs = 32

// validateExecCommand checks the argv of an exec health check: it runs without
// a shell, so each element is one argument.
func validateExecCommand(argv []string, field string, bad func(field, format string, args ...any)) {
	switch {
	case len(argv) == 0:
		bad(field, "required: the command to run, as a list such as [\"pg_isready\"]")
	case len(argv) > maxExecArgs:
		bad(field, "at most %d arguments", maxExecArgs)
	}
	for i, a := range argv {
		if a == "" || strings.ContainsRune(a, 0) {
			bad(field, "argument %d must be non-empty and contain no NUL", i)
		}
	}
}
