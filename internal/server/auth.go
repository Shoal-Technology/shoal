package server

import (
	"context"
	"crypto/x509"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"example.com/shoal/internal/security"
)

// requiredRole maps each gRPC service to the client role allowed to call
// it. Services not listed are denied; Join needs no certificate at all.
var requiredRole = map[string]string{
	"/shoal.v1alpha1.Join/":    "",
	"/shoal.v1alpha1.Control/": security.RoleAdmin,
	"/shoal.v1alpha1.Agent/":   security.RoleNode,
}

type callerKey struct{}

// caller is the authenticated client of an RPC.
type caller struct {
	id   security.Identity
	cert *x509.Certificate
}

func callerFrom(ctx context.Context) (caller, bool) {
	c, ok := ctx.Value(callerKey{}).(caller)
	return c, ok
}

func (s *Server) authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	role, known := "", false
	for prefix, r := range requiredRole {
		if strings.HasPrefix(fullMethod, prefix) {
			role, known = r, true
		}
	}
	if !known {
		return nil, status.Error(codes.PermissionDenied, "unknown service")
	}
	if role == "" {
		return ctx, nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer information")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return nil, status.Error(codes.Unauthenticated, "a client certificate is required")
	}
	cert := info.State.VerifiedChains[0][0]
	// The handshake checked validity, but a connection can outlive its
	// certificate.
	if s.now().After(cert.NotAfter) {
		return nil, status.Error(codes.Unauthenticated, "client certificate expired")
	}
	id, err := security.IdentityFromCert(cert)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if id.Role != role {
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s", id, fullMethod)
	}
	return context.WithValue(ctx, callerKey{}, caller{id: id, cert: cert}), nil
}

func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	actx, err := s.authorize(ctx, info.FullMethod)
	if err != nil {
		s.log.Warn("rejected call", "method", info.FullMethod, "peer", peerAddr(ctx), "err", err)
		return nil, err
	}
	return handler(actx, req)
}

func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	actx, err := s.authorize(ss.Context(), info.FullMethod)
	if err != nil {
		s.log.Warn("rejected call", "method", info.FullMethod, "peer", peerAddr(ss.Context()), "err", err)
		return err
	}
	return handler(srv, &authStream{ServerStream: ss, ctx: actx})
}

type authStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a *authStream) Context() context.Context { return a.ctx }

// peerAddr returns the client's IP address, or "" if unknown.
func peerAddr(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}
	addr := p.Addr.String()
	if i := strings.LastIndex(addr, ":"); i > 0 {
		addr = strings.Trim(addr[:i], "[]")
	}
	return addr
}

// untilExpiry returns how long cert stays valid.
func (s *Server) untilExpiry(cert *x509.Certificate) time.Duration {
	return cert.NotAfter.Sub(s.now())
}
