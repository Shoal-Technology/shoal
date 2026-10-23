package resource

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// Label limits keep labels small: they are stored with every object and
// listed by every get. The rules follow Kubernetes labels.
const (
	MaxLabels     = 32
	maxLabelName  = 63
	maxLabelValue = 63
	maxLabelPrefx = 253
)

var (
	labelName   = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)$`)
	labelValue  = regexp.MustCompile(`^(([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?)$`)
	labelPrefix = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// ValidateLabels checks the keys and values of an object's labels. A key is
// an optional DNS subdomain prefix, a slash, and a name of at most 63
// characters. A value is at most 63 characters and may be empty.
func ValidateLabels(labels map[string]string) error {
	var errs []error
	if len(labels) > MaxLabels {
		errs = append(errs, fmt.Errorf("metadata.labels: %d labels, at most %d", len(labels), MaxLabels))
	}
	for k, v := range labels {
		prefix, name, hasPrefix := strings.Cut(k, "/")
		if !hasPrefix {
			prefix, name = "", k
		}
		switch {
		case hasPrefix && (len(prefix) > maxLabelPrefx || !labelPrefix.MatchString(prefix)):
			errs = append(errs, fmt.Errorf("metadata.labels: key %q: prefix must be a lowercase DNS subdomain of at most %d characters", k, maxLabelPrefx))
		case len(name) == 0 || len(name) > maxLabelName || !labelName.MatchString(name):
			errs = append(errs, fmt.Errorf("metadata.labels: key %q: name must be 1 to %d characters of letters, digits, '-', '_' or '.', starting and ending with a letter or digit", k, maxLabelName))
		}
		if len(v) > maxLabelValue || !labelValue.MatchString(v) {
			errs = append(errs, fmt.Errorf("metadata.labels: value %q of key %q: must be at most %d characters of letters, digits, '-', '_' or '.', starting and ending with a letter or digit (or empty)", v, k, maxLabelValue))
		}
	}
	return errors.Join(errs...)
}

// ObjectLabels returns the labels in an object's metadata.
func ObjectLabels(obj *v1alpha1.Object) map[string]string {
	return metadataOf(obj).GetLabels()
}

// metadataOf returns the metadata of whatever kind obj holds, or nil.
func metadataOf(obj *v1alpha1.Object) *v1alpha1.ObjectMeta {
	switch k := obj.GetKind().(type) {
	case *v1alpha1.Object_Workload:
		return k.Workload.GetMetadata()
	case *v1alpha1.Object_Node:
		return k.Node.GetMetadata()
	case *v1alpha1.Object_Replica:
		return k.Replica.GetMetadata()
	case *v1alpha1.Object_Secret:
		return k.Secret.GetMetadata()
	case *v1alpha1.Object_Config:
		return k.Config.GetMetadata()
	case *v1alpha1.Object_Service:
		return k.Service.GetMetadata()
	case *v1alpha1.Object_Route:
		return k.Route.GetMetadata()
	case *v1alpha1.Object_Volume:
		return k.Volume.GetMetadata()
	case *v1alpha1.Object_Database:
		return k.Database.GetMetadata()
	case *v1alpha1.Object_BuildConfig:
		return k.BuildConfig.GetMetadata()
	case *v1alpha1.Object_Pipeline:
		return k.Pipeline.GetMetadata()
	case *v1alpha1.Object_PipelineRun:
		return k.PipelineRun.GetMetadata()
	case *v1alpha1.Object_Job:
		return k.Job.GetMetadata()
	case *v1alpha1.Object_Build:
		return k.Build.GetMetadata()
	}
	return nil
}

// Requirement is one term of a label selector: key=value, key!=value, key
// (the key exists) or !key (the key is absent).
type Requirement struct {
	Key   string
	Value string
	Op    string // "=", "!=", "exists" or "absent"
}

// ParseSelector parses a comma-separated label selector such as
// "app=blog,tier!=db". "==" is accepted as a synonym for "=".
func ParseSelector(s string) ([]Requirement, error) {
	var out []Requirement
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		switch {
		case strings.HasPrefix(term, "!"):
			key := strings.TrimSpace(term[1:])
			if key == "" || strings.ContainsAny(key, "=!") {
				return nil, fmt.Errorf("selector %q: bad term %q", s, term)
			}
			out = append(out, Requirement{Key: key, Op: "absent"})
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			out = append(out, Requirement{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v), Op: "!="})
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(strings.ReplaceAll(term, "==", "="), "=")
			out = append(out, Requirement{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v), Op: "="})
		default:
			out = append(out, Requirement{Key: term, Op: "exists"})
		}
	}
	for _, r := range out {
		if r.Key == "" {
			return nil, fmt.Errorf("selector %q: empty key", s)
		}
	}
	return out, nil
}

// Matches reports whether labels satisfy every requirement.
func Matches(reqs []Requirement, labels map[string]string) bool {
	for _, r := range reqs {
		v, ok := labels[r.Key]
		switch r.Op {
		case "=":
			if !ok || v != r.Value {
				return false
			}
		case "!=":
			if ok && v == r.Value {
				return false
			}
		case "exists":
			if !ok {
				return false
			}
		case "absent":
			if ok {
				return false
			}
		}
	}
	return true
}

// OwnedByLabel marks an object that a Database controller owns (ADR 0016,
// section 3.2). Owned objects carry the app label so that they show up with
// the app, and this label tells deploy and app delete that the manifest does
// not name them and that the Database controller decides their lifecycle.
const OwnedByLabel = "shoal.dev/owned-by"
