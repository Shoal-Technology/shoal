package server

import (
	"context"
	"errors"
	"sort"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/store"
	"github.com/Shoal-Technology/shoal/internal/store/raftstore"
)

// ClusterStatus reports the raft members as this server sees them, with the
// leader and the indexes of this server. Any server can answer.
func (c *controlService) ClusterStatus(ctx context.Context, _ *v1alpha1.ClusterStatusRequest) (*v1alpha1.ClusterStatusResponse, error) {
	s := c.s
	if s.raft == nil {
		return nil, status.Error(codes.FailedPrecondition, "server does not run the raft store")
	}
	cfg, err := s.raft.Configuration()
	if err != nil {
		return nil, apiError(err)
	}
	commit, err := s.raft.CommitIndex()
	if err != nil {
		return nil, apiError(err)
	}
	leaderAddr, _ := s.raft.Leader()
	var apiAddr string
	if info, err := s.ClusterLeader(ctx); err == nil {
		apiAddr = info.APIAddress
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, apiError(err)
	}
	resp := &v1alpha1.ClusterStatusResponse{
		LeaderApiAddress: apiAddr,
		CommitIndex:      commit,
		AppliedIndex:     s.raft.AppliedIndex(),
		NodeId:           s.cfg.RaftNodeID,
	}
	for _, srv := range cfg.Servers {
		resp.Members = append(resp.Members, &v1alpha1.ClusterMember{
			NodeId:      string(srv.ID),
			RaftAddress: string(srv.Address),
			Voter:       srv.Suffrage == raft.Voter,
			Leader:      leaderAddr != "" && srv.Address == leaderAddr,
		})
	}
	sort.Slice(resp.Members, func(i, j int) bool { return resp.Members[i].NodeId < resp.Members[j].NodeId })
	return resp, nil
}

// RemoveServer removes a raft member. It runs on the leader (a follower
// returns the leader's address, and the CLI retries there). It refuses to
// remove the last voter. Removing the leader itself is allowed: the leader
// steps down once the change commits.
func (c *controlService) RemoveServer(ctx context.Context, req *v1alpha1.RemoveServerRequest) (*v1alpha1.RemoveServerResponse, error) {
	s := c.s
	if s.raft == nil {
		return nil, status.Error(codes.FailedPrecondition, "server does not run the raft store")
	}
	id := req.GetNodeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}
	if !s.raft.IsLeader() {
		return nil, s.notLeader(ctx)
	}
	// One membership change at a time on this node, so the voter count
	// checked below is still true when the change is proposed.
	s.membership.Lock()
	defer s.membership.Unlock()
	cfg, err := s.raft.Configuration()
	if err != nil {
		return nil, apiError(err)
	}
	found, isVoter, voters := false, false, 0
	for _, srv := range cfg.Servers {
		if srv.Suffrage == raft.Voter {
			voters++
		}
		if string(srv.ID) == id {
			found, isVoter = true, srv.Suffrage == raft.Voter
		}
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "no raft member %q", id)
	}
	if isVoter && voters <= 1 {
		return nil, status.Error(codes.FailedPrecondition, "refusing to remove the last voter: the cluster would have no voters")
	}
	err = s.raft.RemoveServer(ctx, raft.ServerID(id))
	if errors.Is(err, raftstore.ErrNotLeader) {
		return nil, s.notLeader(ctx)
	}
	if err != nil {
		return nil, apiError(err)
	}
	idx, err := s.raft.CommitIndex()
	if err != nil {
		return nil, apiError(err)
	}
	s.log.Info("raft member removed", "node", id, "commitIndex", idx)
	return &v1alpha1.RemoveServerResponse{CommitIndex: idx}, nil
}
