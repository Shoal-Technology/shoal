package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/kaiserern/shoal/internal/store/raftstore"
)

// notLeaderMsg is the message of the FailedPrecondition that a write returns
// when it reached a server that is not the raft leader. The leader's API
// address follows leaderAddrMarker (see Server.notLeader). Clients match on
// this prefix, so it must not change.
const notLeaderMsg = "not the leader"

// isNotLeader reports whether err means the write reached a follower. It
// matches the raw store error and the status that apiError produces.
func isNotLeader(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, raftstore.ErrNotLeader) {
		return true
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.FailedPrecondition && st.Message() == notLeaderMsg
}

// unaryLeader rewrites a not-leader error from any unary handler into a
// FailedPrecondition that names the leader's API address (ADR 0014,
// option B). Handlers that do not go through apiError are covered too.
func (s *Server) unaryLeader(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if isNotLeader(err) {
		return nil, s.notLeader(context.WithoutCancel(ctx))
	}
	return resp, err
}

// LeaderRetry returns a unary client interceptor for the CLI. When a call
// fails with a not-leader FailedPrecondition, the call is sent once more to
// the leader's API address from the error, over a new connection that uses
// the same TLS config. The follower rejects the write before it changes
// anything, so the retry cannot apply a write twice.
func LeaderRetry(tc *tls.Config) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		addr, ok := leaderAddrFromError(err)
		if !ok {
			return err
		}
		leader, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
		if err != nil {
			return fmt.Errorf("connect to leader %s: %w", addr, err)
		}
		defer leader.Close()
		return leader.Invoke(ctx, method, req, reply, opts...)
	}
}

// leaderAddrFromError returns the leader's API address named in a not-leader
// error. The leader's own connection has no interceptor, so the retry
// happens at most once.
func leaderAddrFromError(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return "", false
	}
	addr, ok := strings.CutPrefix(st.Message(), notLeaderMsg+"; "+leaderAddrMarker)
	return addr, ok && addr != ""
}
