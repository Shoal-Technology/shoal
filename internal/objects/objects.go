// Package objects converts between store records and API objects.
package objects

import (
	"fmt"
	"maps"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/store"
)

// Specs are encoded deterministically, so an unchanged spec encodes to the
// same bytes and writers can skip no-op updates.
var marshal = proto.MarshalOptions{Deterministic: true}

// Encode encodes a spec or status for the store.
func Encode(m proto.Message) ([]byte, error) {
	b, err := marshal.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", m, err)
	}
	return b, nil
}

// Meta returns the API metadata of a record.
func Meta(r store.Record) *v1alpha1.ObjectMeta {
	return &v1alpha1.ObjectMeta{
		Name:       r.Name,
		Uid:        r.UID,
		Generation: r.Generation,
		Revision:   r.Revision,
		CreatedAt:  timestamppb.New(r.CreatedAt),
		UpdatedAt:  timestamppb.New(r.UpdatedAt),
		Labels:     maps.Clone(r.Labels),
	}
}

func decode(r store.Record, spec, status proto.Message) error {
	if err := proto.Unmarshal(r.Spec, spec); err != nil {
		return fmt.Errorf("decode %s %q spec: %w", r.Kind, r.Name, err)
	}
	if status != nil {
		if err := proto.Unmarshal(r.Status, status); err != nil {
			return fmt.Errorf("decode %s %q status: %w", r.Kind, r.Name, err)
		}
	}
	return nil
}

// Workload decodes a Workload record.
func Workload(r store.Record) (*v1alpha1.Workload, error) {
	w := &v1alpha1.Workload{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindWorkload, Metadata: Meta(r),
		Spec: &v1alpha1.WorkloadSpec{}, Status: &v1alpha1.WorkloadStatus{},
	}
	return w, decode(r, w.Spec, w.Status)
}

// Node decodes a Node record.
func Node(r store.Record) (*v1alpha1.Node, error) {
	n := &v1alpha1.Node{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNode, Metadata: Meta(r),
		Spec: &v1alpha1.NodeSpec{}, Status: &v1alpha1.NodeStatus{},
	}
	return n, decode(r, n.Spec, n.Status)
}

// Replica decodes a Replica record.
func Replica(r store.Record) (*v1alpha1.Replica, error) {
	rp := &v1alpha1.Replica{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindReplica, Metadata: Meta(r),
		Spec: &v1alpha1.ReplicaSpec{}, Status: &v1alpha1.ReplicaStatus{},
	}
	return rp, decode(r, rp.Spec, rp.Status)
}

// Secret decodes a Secret record (the store must decrypt it first).
func Secret(r store.Record) (*v1alpha1.Secret, error) {
	s := &v1alpha1.Secret{ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindSecret, Metadata: Meta(r), Spec: &v1alpha1.SecretSpec{}}
	return s, decode(r, s.Spec, nil)
}

// Config decodes a Config record.
func Config(r store.Record) (*v1alpha1.Config, error) {
	c := &v1alpha1.Config{ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindConfig, Metadata: Meta(r), Spec: &v1alpha1.ConfigSpec{}}
	return c, decode(r, c.Spec, nil)
}

// Service decodes a Service record.
func Service(r store.Record) (*v1alpha1.Service, error) {
	s := &v1alpha1.Service{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindService, Metadata: Meta(r),
		Spec: &v1alpha1.ServiceSpec{}, Status: &v1alpha1.ServiceStatus{},
	}
	return s, decode(r, s.Spec, s.Status)
}

// Route decodes a Route record.
func Route(r store.Record) (*v1alpha1.Route, error) {
	rt := &v1alpha1.Route{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindRoute, Metadata: Meta(r),
		Spec: &v1alpha1.RouteSpec{}, Status: &v1alpha1.RouteStatus{},
	}
	return rt, decode(r, rt.Spec, rt.Status)
}

// Volume decodes a Volume record.
func Volume(r store.Record) (*v1alpha1.Volume, error) {
	v := &v1alpha1.Volume{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindVolume, Metadata: Meta(r),
		Spec: &v1alpha1.VolumeSpec{}, Status: &v1alpha1.VolumeStatus{},
	}
	return v, decode(r, v.Spec, v.Status)
}

// Database decodes a Database record.
func Database(r store.Record) (*v1alpha1.Database, error) {
	d := &v1alpha1.Database{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDatabase, Metadata: Meta(r),
		Spec: &v1alpha1.DatabaseSpec{}, Status: &v1alpha1.DatabaseStatus{},
	}
	return d, decode(r, d.Spec, d.Status)
}

// Certificate decodes a Certificate record (the store must decrypt it first).
func Certificate(r store.Record) (*v1alpha1.Certificate, error) {
	c := &v1alpha1.Certificate{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCertificate, Metadata: Meta(r),
		Spec: &v1alpha1.CertificateSpec{}, Status: &v1alpha1.CertificateStatus{},
	}
	return c, decode(r, c.Spec, c.Status)
}

// AcmeAccount decodes an AcmeAccount record (the store must decrypt it first).
func AcmeAccount(r store.Record) (*v1alpha1.AcmeAccount, error) {
	a := &v1alpha1.AcmeAccount{
		ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindAcmeAccount, Metadata: Meta(r),
		Spec: &v1alpha1.AcmeAccountSpec{}, Status: &v1alpha1.AcmeAccountStatus{},
	}
	return a, decode(r, a.Spec, a.Status)
}

// AcmeChallenge decodes an AcmeChallenge record (the store must decrypt it first).
func AcmeChallenge(r store.Record) (*v1alpha1.AcmeChallenge, error) {
	c := &v1alpha1.AcmeChallenge{ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindAcmeChallenge, Metadata: Meta(r), Spec: &v1alpha1.AcmeChallengeSpec{}}
	return c, decode(r, c.Spec, nil)
}

// JoinTokenSpec decodes a JoinToken record's spec.
func JoinTokenSpec(r store.Record) (*v1alpha1.JoinTokenSpec, error) {
	s := &v1alpha1.JoinTokenSpec{}
	return s, decode(r, s, nil)
}

// Template returns what a replica of a workload with this spec runs: the
// spec without the fields that only concern the workload as a whole
// (replicas, update), so scaling or changing the update strategy never
// changes existing replicas.
func Template(spec *v1alpha1.WorkloadSpec) *v1alpha1.WorkloadSpec {
	t := proto.CloneOf(spec)
	t.Replicas, t.Update = nil, nil
	return t
}
