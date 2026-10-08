package resource

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	"example.com/shoal/api/v1alpha1"
)

// docSeparator matches a YAML document separator line.
var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*(#.*)?$`)

// Decode parses a YAML manifest with one or more documents into objects.
// Fields follow the protobuf JSON names (lowerCamelCase); unknown fields are
// rejected so typos fail loudly. A missing apiVersion means v1alpha1.
func Decode(data []byte) ([]*v1alpha1.Object, error) {
	var out []*v1alpha1.Object
	for i, doc := range docSeparator.Split(string(data), -1) {
		obj, err := decodeDoc([]byte(doc))
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if obj != nil {
			out = append(out, obj)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("manifest contains no objects")
	}
	return out, nil
}

func decodeDoc(doc []byte) (*v1alpha1.Object, error) {
	j, err := yaml.YAMLToJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if s := strings.TrimSpace(string(j)); s == "null" || s == "" {
		return nil, nil // empty document or only comments
	}
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(j, &head); err != nil {
		return nil, fmt.Errorf("document must be a mapping with apiVersion and kind: %w", err)
	}
	if head.APIVersion != "" && head.APIVersion != v1alpha1.APIVersion {
		return nil, fmt.Errorf("unsupported apiVersion %q (want %s)", head.APIVersion, v1alpha1.APIVersion)
	}
	var obj *v1alpha1.Object
	var msg proto.Message
	stringy := "" // spec field whose values must be strings
	switch head.Kind {
	case v1alpha1.KindWorkload:
		w := &v1alpha1.Workload{}
		obj, msg, stringy = &v1alpha1.Object{Kind: &v1alpha1.Object_Workload{Workload: w}}, w, "resources"
	case v1alpha1.KindSecret:
		s := &v1alpha1.Secret{}
		obj, msg, stringy = &v1alpha1.Object{Kind: &v1alpha1.Object_Secret{Secret: s}}, s, "data"
	case v1alpha1.KindConfig:
		c := &v1alpha1.Config{}
		obj, msg, stringy = &v1alpha1.Object{Kind: &v1alpha1.Object_Config{Config: c}}, c, "data"
	case v1alpha1.KindService:
		s := &v1alpha1.Service{}
		obj, msg = &v1alpha1.Object{Kind: &v1alpha1.Object_Service{Service: s}}, s
	case "":
		return nil, fmt.Errorf("kind is required")
	default:
		return nil, fmt.Errorf("kind %q cannot be applied (supported: Workload, Service, Secret, Config)", head.Kind)
	}
	if stringy != "" {
		if j, err = valuesAsStrings(j, stringy); err != nil {
			return nil, err
		}
	}
	if err := protojson.Unmarshal(j, msg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", head.Kind, err)
	}
	return obj, nil
}

// valuesAsStrings turns number and boolean values of spec.<field> into
// strings, so `cpu: 0.5` or `port: 5432` in a Config work like quoted
// strings (YAML parses them as numbers, but the API fields are strings).
func valuesAsStrings(j []byte, field string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(j, &m); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	spec, _ := m["spec"].(map[string]any)
	values, _ := spec[field].(map[string]any)
	changed := false
	for k, v := range values {
		switch v := v.(type) {
		case float64:
			values[k], changed = strconv.FormatFloat(v, 'f', -1, 64), true
		case bool:
			values[k], changed = strconv.FormatBool(v), true
		}
	}
	if !changed {
		return j, nil
	}
	return json.Marshal(m)
}

// EncodeYAML renders a resource (e.g. a *v1alpha1.Workload) as YAML using
// the same field names Decode accepts.
func EncodeYAML(m proto.Message) ([]byte, error) {
	j, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	y, err := yaml.JSONToYAML(j)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return y, nil
}

// Inner returns the resource held by obj (e.g. its *v1alpha1.Workload), or
// nil if obj is empty.
func Inner(obj *v1alpha1.Object) proto.Message {
	switch k := obj.GetKind().(type) {
	case *v1alpha1.Object_Workload:
		return k.Workload
	case *v1alpha1.Object_Node:
		return k.Node
	case *v1alpha1.Object_Replica:
		return k.Replica
	case *v1alpha1.Object_Secret:
		return k.Secret
	case *v1alpha1.Object_Config:
		return k.Config
	case *v1alpha1.Object_Service:
		return k.Service
	}
	return nil
}
