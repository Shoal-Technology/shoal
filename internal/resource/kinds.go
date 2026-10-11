package resource

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// MaxDataBytes bounds the data of one Secret or Config: they travel to
// nodes in every assignment snapshot.
const MaxDataBytes = 256 << 10

// dataKey is a valid key of a Secret or Config: usable as a file name and,
// with a valueFrom reference, as a source of an environment variable.
var dataKey = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// Prepare defaults and validates an applied object. It returns the
// object's kind, name and the spec to store. Labels in the metadata are
// validated too; the caller stores them with the spec.
func Prepare(obj *v1alpha1.Object) (kind, name string, spec proto.Message, err error) {
	kind, name, spec, err = prepare(obj)
	return kind, name, spec, errors.Join(err, ValidateLabels(ObjectLabels(obj)))
}

func prepare(obj *v1alpha1.Object) (kind, name string, spec proto.Message, err error) {
	switch k := obj.GetKind().(type) {
	case *v1alpha1.Object_Workload:
		w := proto.CloneOf(k.Workload)
		DefaultWorkload(w)
		return v1alpha1.KindWorkload, w.Metadata.GetName(), w.Spec, ValidateWorkload(w)
	case *v1alpha1.Object_Secret:
		name := k.Secret.GetMetadata().GetName()
		s := proto.CloneOf(k.Secret.GetSpec())
		if s == nil {
			s = &v1alpha1.SecretSpec{}
		}
		return v1alpha1.KindSecret, name, s, validateData(name, s.GetData())
	case *v1alpha1.Object_Config:
		name := k.Config.GetMetadata().GetName()
		s := proto.CloneOf(k.Config.GetSpec())
		if s == nil {
			s = &v1alpha1.ConfigSpec{}
		}
		return v1alpha1.KindConfig, name, s, validateData(name, s.GetData())
	case *v1alpha1.Object_Service:
		name := k.Service.GetMetadata().GetName()
		s := proto.CloneOf(k.Service.GetSpec())
		if s == nil {
			s = &v1alpha1.ServiceSpec{}
		}
		DefaultService(s)
		return v1alpha1.KindService, name, s, ValidateService(name, s)
	case *v1alpha1.Object_Route:
		name := k.Route.GetMetadata().GetName()
		s := proto.CloneOf(k.Route.GetSpec())
		if s == nil {
			s = &v1alpha1.RouteSpec{}
		}
		DefaultRoute(s)
		return v1alpha1.KindRoute, name, s, ValidateRoute(name, s)
	case *v1alpha1.Object_Database:
		name := k.Database.GetMetadata().GetName()
		s := proto.CloneOf(k.Database.GetSpec())
		if s == nil {
			s = &v1alpha1.DatabaseSpec{}
		}
		DefaultDatabase(s)
		return v1alpha1.KindDatabase, name, s, ValidateDatabase(name, s)
	case *v1alpha1.Object_Volume:
		name := k.Volume.GetMetadata().GetName()
		s := proto.CloneOf(k.Volume.GetSpec())
		if s == nil {
			s = &v1alpha1.VolumeSpec{}
		}
		DefaultVolume(s)
		return v1alpha1.KindVolume, name, s, ValidateVolume(name, s)
	}
	return "", "", nil, errors.New("only Workload, Service, Route, Volume, Database, Secret and Config objects can be applied")
}

func validateData(name string, data map[string]string) error {
	var errs []error
	if !ValidName(name, 63) {
		errs = append(errs, fmt.Errorf("metadata.name: must be a lowercase DNS label of at most 63 characters"))
	}
	size := 0
	for k, v := range data {
		if !dataKey.MatchString(k) || k == "." || k == ".." {
			errs = append(errs, fmt.Errorf("data: invalid key %q (letters, digits, '-', '_', '.')", k))
		}
		size += len(k) + len(v)
	}
	if size > MaxDataBytes {
		errs = append(errs, fmt.Errorf("data: %d bytes, at most %d", size, MaxDataBytes))
	}
	return errors.Join(errs...)
}

// DefaultService fills in target ports and protocols.
func DefaultService(s *v1alpha1.ServiceSpec) {
	for _, p := range s.Ports {
		if p.TargetPort == 0 {
			p.TargetPort = p.Port
		}
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
	}
}

// ValidateService checks a defaulted Service spec.
func ValidateService(name string, s *v1alpha1.ServiceSpec) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}
	if !ValidName(name, 63) {
		bad("metadata.name", "must be a lowercase DNS label of at most 63 characters")
	}
	if !ValidName(s.GetSelector(), MaxWorkloadNameLen) {
		bad("spec.selector", "must name a workload")
	}
	if len(s.GetPorts()) == 0 {
		bad("spec.ports", "at least one port is required")
	}
	seen := map[string]bool{}
	for i, p := range s.GetPorts() {
		f := fmt.Sprintf("spec.ports[%d]", i)
		if !validPort(p.GetPort()) || !validPort(p.GetTargetPort()) {
			bad(f, "port and targetPort must be between 1 and 65535")
		}
		if p.GetProtocol() != "tcp" && p.GetProtocol() != "udp" {
			bad(f+".protocol", "must be tcp or udp")
		}
		key := fmt.Sprintf("%d/%s", p.GetPort(), p.GetProtocol())
		if seen[key] {
			bad(f, "duplicate port %s", key)
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}

// validateEnvAndFiles checks the Secret and Config references of a
// workload spec.
func validateEnvAndFiles(s *v1alpha1.WorkloadSpec, bad func(field, format string, args ...any)) {
	for i, e := range s.GetEnv() {
		f := fmt.Sprintf("spec.env[%d]", i)
		src := e.GetValueFrom()
		if src == nil {
			continue
		}
		if e.GetValue() != "" {
			bad(f, "set either value or valueFrom, not both")
		}
		switch ref := firstRef(src.GetSecret(), src.GetConfig()); {
		case ref == nil || (src.GetSecret() != nil && src.GetConfig() != nil):
			bad(f+".valueFrom", "set exactly one of secret or config")
		case !ValidName(ref.GetName(), 63):
			bad(f+".valueFrom", "invalid name %q", ref.GetName())
		case !dataKey.MatchString(ref.GetKey()):
			bad(f+".valueFrom", "invalid key %q", ref.GetKey())
		}
	}
	paths := map[string]bool{}
	for i, m := range s.GetFiles() {
		f := fmt.Sprintf("spec.files[%d]", i)
		p := m.GetPath()
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" {
			bad(f+".path", "must be a clean absolute directory, not /")
		}
		if paths[p] {
			bad(f+".path", "duplicate path %s", p)
		}
		paths[p] = true
		if (m.GetSecret() == "") == (m.GetConfig() == "") {
			bad(f, "set exactly one of secret or config")
		} else if !ValidName(m.GetSecret()+m.GetConfig(), 63) {
			bad(f, "invalid name %q", m.GetSecret()+m.GetConfig())
		}
	}
}

func firstRef(refs ...*v1alpha1.KeyRef) *v1alpha1.KeyRef {
	for _, r := range refs {
		if r != nil {
			return r
		}
	}
	return nil
}
