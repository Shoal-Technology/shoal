package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/store"
)

var encode = objects.Encode

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// apiError converts an internal error into a gRPC status. Store sentinel
// errors map to their codes; anything else is Internal.
func apiError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case isNotLeader(err):
		// Callers name the leader's address; see unaryLeader and writeRPCError.
		return status.Error(codes.FailedPrecondition, notLeaderMsg)
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, store.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Internal, err.Error())
}
