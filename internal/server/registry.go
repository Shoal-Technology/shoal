package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/registry"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// RegistryOptions configures the embedded registry (ADR 0020).
type RegistryOptions struct {
	// Dir holds the registry's blobs and manifests; default <data-dir>/registry.
	Dir string
	// Names are extra DNS names or IPs for the listener's certificate.
	Names []string
	// AnonymousPull lets requests without a client certificate pull.
	AnonymousPull bool
}

// RegistryTLS returns the TLS config of the registry listener. The server
// certificate comes from the cluster CA. A client certificate is verified
// against the same CA when one is presented; registry.New decides what it
// may do.
func (s *Server) RegistryTLS(names []string) (*tls.Config, error) {
	cert, err := s.servingCert(names)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.Cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}, nil
}

// RegistryHandler returns the registry's HTTP handler. Node checks and tag
// rows go through the store.
func (s *Server) RegistryHandler(ctx context.Context, opts RegistryOptions) (http.Handler, error) {
	dir := opts.Dir
	if dir == "" {
		dir = filepath.Join(s.cfg.DataDir, "registry")
	}
	return registry.New(ctx, registry.Config{
		Dir:           dir,
		AnonymousPull: opts.AnonymousPull,
		NodeExists:    s.registryNodeExists,
		BuildScope:    s.builds.scope,
		RecordTag:     s.recordImageTag,
		ListTags:      s.listImageTags,
	})
}

func (s *Server) registryNodeExists(ctx context.Context, name string) (bool, error) {
	_, err := s.store.Get(ctx, v1alpha1.KindNode, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// recordImageTag writes the ImageTag row for a tag, moving it when the tag
// already exists. An unchanged row is not rewritten.
func (s *Server) recordImageTag(ctx context.Context, t registry.Tag) error {
	if t.Digest == "" {
		return errors.New("image tag: manifest has no digest")
	}
	name := t.Repository + ":" + t.Tag
	spec, err := proto.MarshalOptions{Deterministic: true}.Marshal(&v1alpha1.ImageTagSpec{
		Repository: t.Repository,
		Tag:        t.Tag,
		Digest:     t.Digest,
		MediaType:  t.MediaType,
	})
	if err != nil {
		return err
	}
	// Retry on a concurrent writer (a conflict or a racing create).
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := s.store.Get(ctx, v1alpha1.KindImageTag, name)
		if errors.Is(err, store.ErrNotFound) {
			_, err = s.store.Create(ctx, store.Record{Kind: v1alpha1.KindImageTag, Name: name, Spec: spec})
			if errors.Is(err, store.ErrExists) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		if bytes.Equal(rec.Spec, spec) {
			return nil
		}
		_, err = s.store.Update(ctx, v1alpha1.KindImageTag, name, spec, rec.Generation)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("image tag %s: too many concurrent updates", name)
}

func (s *Server) listImageTags(ctx context.Context) ([]registry.Tag, error) {
	recs, err := s.store.List(ctx, v1alpha1.KindImageTag)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Tag, 0, len(recs))
	for _, r := range recs {
		var sp v1alpha1.ImageTagSpec
		if err := proto.Unmarshal(r.Spec, &sp); err != nil {
			return nil, fmt.Errorf("image tag %s: %w", r.Name, err)
		}
		out = append(out, registry.Tag{Repository: sp.Repository, Tag: sp.Tag, Digest: sp.Digest, MediaType: sp.MediaType})
	}
	return out, nil
}
