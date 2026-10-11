package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/objects"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/store"
)

// errInvalidToken is returned for unknown ids and wrong secrets alike, so
// callers can't probe which token ids exist.
var errInvalidToken = status.Error(codes.Unauthenticated, "invalid join token")

type joinService struct {
	v1alpha1.UnimplementedJoinServer
	s *Server
}

func (j *joinService) Join(ctx context.Context, req *v1alpha1.JoinRequest) (*v1alpha1.JoinResponse, error) {
	name := req.GetNodeName()
	if !resource.ValidName(name, 63) {
		return nil, status.Error(codes.InvalidArgument, "node name must be a lowercase DNS label (a-z, 0-9, '-') of at most 63 characters")
	}
	id, secret, err := security.ParseToken(req.GetToken())
	if err != nil {
		return nil, j.reject(ctx, name, "", errInvalidToken)
	}
	now := j.s.now()
	// Sign before consuming the token, so a bad request doesn't use it up.
	cert, err := j.s.ca.SignCSR(req.GetCsr(), security.Identity{Name: name, Role: security.RoleNode}, now, security.NodeValidity)
	if err != nil {
		return nil, j.reject(ctx, name, id, status.Error(codes.InvalidArgument, err.Error()))
	}
	if err := j.consume(ctx, id, secret, now); err != nil {
		return nil, j.reject(ctx, name, id, err)
	}

	st, err := encode(&v1alpha1.NodeStatus{Phase: NodeNotReady})
	if err != nil {
		return nil, apiError(err)
	}
	_, err = j.s.store.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Status: st})
	rejoin := errors.Is(err, store.ErrExists)
	if err != nil && !rejoin {
		return nil, apiError(fmt.Errorf("register node %s: %w", name, err))
	}
	j.s.log.Info("node joined", "node", name, "token", id, "rejoin", rejoin, "peer", peerAddr(ctx))
	return &v1alpha1.JoinResponse{Certificate: cert.Raw, CaCertificate: j.s.ca.Cert.Raw}, nil
}

// consume checks the token and counts one use of it.
func (j *joinService) consume(ctx context.Context, id, secret string, now time.Time) error {
	for range casRetries {
		rec, err := j.s.store.Get(ctx, v1alpha1.KindJoinToken, id)
		if errors.Is(err, store.ErrNotFound) {
			return errInvalidToken
		}
		if err != nil {
			return apiError(err)
		}
		spec, err := objects.JoinTokenSpec(rec)
		if err != nil {
			return apiError(err)
		}
		switch {
		case !security.SecretMatches(spec.GetSecretHash(), secret):
			return errInvalidToken
		case !now.Before(spec.GetExpiresAt().AsTime()):
			return status.Error(codes.Unauthenticated, "join token expired")
		case spec.GetMaxUses() > 0 && spec.GetUses() >= spec.GetMaxUses():
			return status.Error(codes.Unauthenticated, "join token has no uses left")
		}
		spec.Uses++
		b, err := encode(spec)
		if err != nil {
			return apiError(err)
		}
		_, err = j.s.store.Update(ctx, v1alpha1.KindJoinToken, id, b, rec.Generation)
		if errors.Is(err, store.ErrConflict) {
			continue // used concurrently: re-check the limit
		}
		return apiError(err)
	}
	return status.Error(codes.Aborted, "join token is being used concurrently, try again")
}

func (j *joinService) reject(ctx context.Context, node, tokenID string, err error) error {
	j.s.log.Warn("join rejected", "node", node, "token", tokenID, "peer", peerAddr(ctx), "err", err)
	return err
}

// gcTokens deletes join tokens that are expired or used up.
func (s *Server) gcTokens(ctx context.Context) error {
	recs, err := s.store.List(ctx, v1alpha1.KindJoinToken)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for _, r := range recs {
		spec, err := objects.JoinTokenSpec(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		expired := !now.Before(spec.GetExpiresAt().AsTime())
		usedUp := spec.GetMaxUses() > 0 && spec.GetUses() >= spec.GetMaxUses()
		if !expired && !usedUp {
			continue
		}
		// Generation check: don't delete a token that was just used.
		err = s.store.Delete(ctx, v1alpha1.KindJoinToken, r.Name, r.Generation)
		if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
			errs = append(errs, err)
			continue
		}
		s.log.Debug("join token removed", "token", r.Name, "expired", expired, "usedUp", usedUp)
	}
	return errors.Join(errs...)
}
