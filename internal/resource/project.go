package resource

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// maxProjectText bounds the displayName and description of a Project.
const maxProjectText = 200

// ValidateProject checks a Project's name and spec.
func ValidateProject(name string, s *v1alpha1.ProjectSpec) error {
	var errs []error
	if err := tenancy.CheckName(name); err != nil {
		errs = append(errs, fmt.Errorf("metadata.name: %w", err))
	}
	if len(s.GetDisplayName()) > maxProjectText {
		errs = append(errs, fmt.Errorf("spec.displayName: at most %d bytes", maxProjectText))
	}
	if len(s.GetDescription()) > maxProjectText {
		errs = append(errs, fmt.Errorf("spec.description: at most %d bytes", maxProjectText))
	}
	return errors.Join(errs...)
}

// Unlimited marks a quota field that is not set.
const Unlimited int64 = -1

// Limits are the parsed hard limits of a ResourceQuota. CPU is in
// millicores, Memory and Storage in bytes, Workloads a count. Each is Unlimited
// when the quota does not set it.
type Limits struct {
	CPU, Memory, Storage, Workloads int64
}

// ValidateResourceQuota checks a quota's name and hard limits.
func ValidateResourceQuota(name string, s *v1alpha1.ResourceQuotaSpec) error {
	var errs []error
	if !ValidName(name, 63) {
		errs = append(errs, fmt.Errorf("metadata.name: must be a lowercase DNS label of at most 63 characters"))
	}
	if _, err := ParseLimits(s); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ParseLimits parses the hard limits of a quota spec.
func ParseLimits(s *v1alpha1.ResourceQuotaSpec) (Limits, error) {
	lim := Limits{CPU: Unlimited, Memory: Unlimited, Storage: Unlimited, Workloads: Unlimited}
	var errs []error
	if q := strings.TrimSpace(s.GetCpu()); q != "" {
		v, err := ParseCPU(q)
		if err != nil {
			errs = append(errs, fmt.Errorf("spec.cpu: %w", err))
		} else {
			lim.CPU = v
		}
	}
	if q := strings.TrimSpace(s.GetMemory()); q != "" {
		v, err := ParseMemory(q)
		if err != nil {
			errs = append(errs, fmt.Errorf("spec.memory: %w", err))
		} else {
			lim.Memory = v
		}
	}
	if q := strings.TrimSpace(s.GetStorage()); q != "" {
		v, err := ParseMemory(q)
		if err != nil {
			errs = append(errs, fmt.Errorf("spec.storage: %w", err))
		} else {
			lim.Storage = v
		}
	}
	if s.Workloads != nil {
		if *s.Workloads < 0 {
			errs = append(errs, fmt.Errorf("spec.workloads: must not be negative"))
		} else {
			lim.Workloads = int64(*s.Workloads)
		}
	}
	return lim, errors.Join(errs...)
}

// Usage is what project-scoped objects use, in the units of Limits.
type Usage struct {
	CPU, Memory, Storage, Workloads int64
}

// Add returns u plus v.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.CPU + v.CPU, u.Memory + v.Memory, u.Storage + v.Storage, u.Workloads + v.Workloads}
}

// Sub returns u minus v.
func (u Usage) Sub(v Usage) Usage {
	return Usage{u.CPU - v.CPU, u.Memory - v.Memory, u.Storage - v.Storage, u.Workloads - v.Workloads}
}

// UsageOf returns what one object's spec uses. A Workload uses replicas times
// its requested CPU and memory (Shoal enforces them as hard limits), and
// counts once. A Volume uses its size. Other specs use nothing.
func UsageOf(spec proto.Message) (Usage, error) {
	switch s := spec.(type) {
	case *v1alpha1.WorkloadSpec:
		replicas := int64(1)
		if s.Replicas != nil {
			replicas = int64(*s.Replicas)
		}
		var u Usage
		u.Workloads = 1
		if q := strings.TrimSpace(s.GetResources().GetCpu()); q != "" {
			cpu, err := ParseCPU(q)
			if err != nil {
				return Usage{}, err
			}
			u.CPU = cpu * replicas
		}
		if q := strings.TrimSpace(s.GetResources().GetMemory()); q != "" {
			mem, err := ParseMemory(q)
			if err != nil {
				return Usage{}, err
			}
			u.Memory = mem * replicas
		}
		return u, nil
	case *v1alpha1.VolumeSpec:
		if q := strings.TrimSpace(s.GetSize()); q != "" {
			size, err := ParseMemory(q)
			if err != nil {
				return Usage{}, err
			}
			return Usage{Storage: size}, nil
		}
	}
	return Usage{}, nil
}

// CheckQuota refuses a change that takes project past the limits of quota.
// before is the usage now and after is the usage once the change is applied.
// A resource is refused only when the change raises it above its limit, so a
// change that lowers usage is never refused, even when the project is already
// over the limit. The error names each refused resource.
func CheckQuota(project, quota string, lim Limits, before, after Usage) error {
	type check struct {
		name       string
		limit      int64
		used, want int64
		show       func(int64) string
	}
	checks := []check{
		{"cpu", lim.CPU, before.CPU, after.CPU, showCPU},
		{"memory", lim.Memory, before.Memory, after.Memory, showBytes},
		{"storage", lim.Storage, before.Storage, after.Storage, showBytes},
		{"workloads", lim.Workloads, before.Workloads, after.Workloads, func(n int64) string { return fmt.Sprint(n) }},
	}
	var errs []error
	for _, c := range checks {
		if c.limit == Unlimited || c.want <= c.limit || c.want <= c.used {
			continue
		}
		errs = append(errs, fmt.Errorf("quota %q in project %q: %s would use %s (now %s), hard limit is %s",
			quota, project, c.name, c.show(c.want), c.show(c.used), c.show(c.limit)))
	}
	return errors.Join(errs...)
}

func showCPU(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}

func showBytes(b int64) string {
	const gi, mi = 1 << 30, 1 << 20
	switch {
	case b >= gi && b%gi == 0:
		return fmt.Sprintf("%dGi", b/gi)
	case b >= mi && b%mi == 0:
		return fmt.Sprintf("%dMi", b/mi)
	}
	return fmt.Sprintf("%d bytes", b)
}
