package resource

import (
	"errors"
	"fmt"
	"regexp"

	"example.com/shoal/api/v1alpha1"
)

// Volume limits and values (ADR 0016, section 1).
const (
	// MinVolumeBytes leaves room for the ext4 journal and metadata on the
	// loop backend.
	MinVolumeBytes = 16 << 20

	RetentionRetain = "Retain"
	RetentionDelete = "Delete"

	// AccessRWO is the only access mode in v1.
	AccessRWO = "RWO"
)

// volumeMode is an octal permission such as 0700.
var volumeMode = regexp.MustCompile(`^0[0-7]{3}$`)

// DefaultVolume fills in access mode, mode, owner and retention.
func DefaultVolume(s *v1alpha1.VolumeSpec) {
	if s.AccessMode == "" {
		s.AccessMode = AccessRWO
	}
	if s.Mode == "" {
		s.Mode = "0700"
	}
	if s.Owner == nil {
		s.Owner = &v1alpha1.VolumeOwner{}
	}
	if s.Retention == "" {
		s.Retention = RetentionRetain
	}
}

// VolumeBytes returns the size of a defaulted, validated Volume in bytes.
func VolumeBytes(s *v1alpha1.VolumeSpec) (int64, error) {
	return ParseMemory(s.GetSize())
}

// ValidateVolume checks a defaulted Volume spec and reports every problem
// found, each prefixed with its field path.
func ValidateVolume(name string, s *v1alpha1.VolumeSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, 63) {
		bad("metadata.name", "must be a lowercase DNS label of at most 63 characters")
	}
	switch size := s.GetSize(); {
	case size == "":
		bad("spec.size", "required, for example 10Gi")
	default:
		if v, err := ParseMemory(size); err != nil {
			bad("spec.size", "%v", err)
		} else if v < MinVolumeBytes {
			bad("spec.size", "must be at least 16Mi")
		}
	}
	if s.GetAccessMode() != AccessRWO {
		bad("spec.accessMode", "only RWO is supported in v1")
	}
	if o := s.GetOwner(); o.GetUid() < 0 || o.GetGid() < 0 {
		bad("spec.owner", "uid and gid must not be negative")
	}
	if !volumeMode.MatchString(s.GetMode()) {
		bad("spec.mode", "must be an octal mode such as \"0700\"")
	}
	switch s.GetRetention() {
	case RetentionRetain, RetentionDelete:
	default:
		bad("spec.retention", "must be Retain or Delete")
	}
	return errors.Join(errs...)
}
