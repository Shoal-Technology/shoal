package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/security"
	"github.com/kaiserern/shoal/internal/store"
	"github.com/kaiserern/shoal/internal/store/raftstore"
)

// The leader record is written by each server when it gains leadership. It
// is replicated like any other record, so every server can tell clients
// where the leader's API is.
const (
	kindCluster = "Cluster"
	leaderName  = "leader"

	// leaderAddrMarker precedes the leader's API address in the message of
	// a FailedPrecondition returned by a follower.
	leaderAddrMarker = "leader API address is "

	// joinTimeout bounds how long a joining server keeps trying to reach
	// the cluster before it fails to start.
	joinTimeout = 2 * time.Minute
)

// LeaderInfo is the cluster leader record.
type LeaderInfo struct {
	// APIAddress is the leader's gRPC API address (host:port).
	APIAddress string `json:"apiAddress"`
	// RaftNodeID and RaftAddress identify the leader in the raft cluster.
	RaftNodeID  string `json:"raftNodeID"`
	RaftAddress string `json:"raftAddress"`
}

// publishLeader writes this server's leader record. It is called when the
// server gains leadership, before the leader tasks start.
func (s *Server) publishLeader(ctx context.Context) error {
	info := LeaderInfo{APIAddress: s.cfg.AdvertiseAddress, RaftNodeID: s.cfg.RaftNodeID}
	if s.raft != nil {
		info.RaftAddress = string(s.raft.RaftAddr())
	}
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, err = s.store.Create(ctx, store.Record{Kind: kindCluster, Name: leaderName, Status: b})
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrExists) {
		return fmt.Errorf("write leader record: %w", err)
	}
	if _, err := s.store.UpdateStatus(ctx, kindCluster, leaderName, b); err != nil {
		return fmt.Errorf("update leader record: %w", err)
	}
	return nil
}

// ClusterLeader returns the leader record as this server currently sees it.
// It returns store.ErrNotFound if no leader has written one yet.
func (s *Server) ClusterLeader(ctx context.Context) (LeaderInfo, error) {
	rec, err := s.store.Get(ctx, kindCluster, leaderName)
	if err != nil {
		return LeaderInfo{}, err
	}
	var info LeaderInfo
	if err := json.Unmarshal(rec.Status, &info); err != nil {
		return LeaderInfo{}, fmt.Errorf("decode leader record: %w", err)
	}
	return info, nil
}

// notLeader is the error for a JoinCluster that reached a follower. It names
// the leader's API address so the caller can retry there.
func (s *Server) notLeader(ctx context.Context) error {
	info, err := s.ClusterLeader(ctx)
	if err != nil || info.APIAddress == "" {
		return status.Error(codes.FailedPrecondition, "not the leader; no leader is known yet")
	}
	return status.Error(codes.FailedPrecondition, "not the leader; "+leaderAddrMarker+info.APIAddress)
}

// JoinCluster adds a server to the raft cluster as a voter. It is idempotent:
// asking for a voter that is already present at the same address succeeds.
func (c *controlService) JoinCluster(ctx context.Context, req *v1alpha1.JoinClusterRequest) (*v1alpha1.JoinClusterResponse, error) {
	s := c.s
	if s.raft == nil {
		return nil, status.Error(codes.FailedPrecondition, "server does not run the raft store")
	}
	if req.GetNodeId() == "" || req.GetRaftAddr() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id and raft_addr are required")
	}
	if !s.raft.IsLeader() {
		return nil, s.notLeader(ctx)
	}
	err := s.raft.AddVoter(ctx, raft.ServerID(req.GetNodeId()), raft.ServerAddress(req.GetRaftAddr()))
	if errors.Is(err, raftstore.ErrNotLeader) {
		return nil, s.notLeader(ctx)
	}
	if err != nil {
		return nil, apiError(err)
	}
	idx, err := s.raft.CommitIndex()
	if err != nil {
		return nil, apiError(fmt.Errorf("read commit index: %w", err))
	}
	s.log.Info("raft voter joined", "node", req.GetNodeId(), "raftAddr", req.GetRaftAddr())
	return &v1alpha1.JoinClusterResponse{CommitIndex: idx}, nil
}

// joinCluster asks the cluster at Config.RaftJoinAddr to add this server as
// a voter. It retries with backoff until it succeeds, the join timeout
// passes, or the error is permanent. It follows a FailedPrecondition to the
// leader's address once. It returns the leader's commit index at the join.
func (s *Server) joinCluster(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	admin, err := s.adminTLS()
	if err != nil {
		return 0, err
	}
	req := &v1alpha1.JoinClusterRequest{NodeId: s.cfg.RaftNodeID, RaftAddr: s.raftAdvertise()}
	target := s.cfg.RaftJoinAddr
	followed := false
	backoff := 200 * time.Millisecond
	for {
		resp, err := joinOnce(ctx, admin, target, req)
		if err == nil {
			s.log.Info("joined raft cluster", "via", target, "raftAddr", req.RaftAddr, "commitIndex", resp.GetCommitIndex())
			return resp.GetCommitIndex(), nil
		}
		st := status.Convert(err)
		if st.Code() == codes.FailedPrecondition && !followed {
			if addr, ok := strings.CutPrefix(st.Message(), "not the leader; "+leaderAddrMarker); ok && addr != "" {
				s.log.Info("following join to leader", "from", target, "leader", addr)
				target, followed = addr, true
				continue
			}
		}
		switch st.Code() {
		case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied:
			return 0, fmt.Errorf("join %s: %w", target, err)
		}
		s.log.Warn("join failed, retrying", "via", target, "err", err)
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("join cluster via %s: giving up after %s: %w", s.cfg.RaftJoinAddr, joinTimeout, err)
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func joinOnce(ctx context.Context, tc *tls.Config, target string, req *v1alpha1.JoinClusterRequest) (*v1alpha1.JoinClusterResponse, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return v1alpha1.NewControlClient(conn).JoinCluster(ctx, req)
}

// adminTLS loads this server's admin credentials, which are issued by the
// cluster CA. They let it call the Control API on other servers.
func (s *Server) adminTLS() (*tls.Config, error) {
	c, err := security.LoadClientConfig(AdminConfigPath(s.cfg.DataDir))
	if err != nil {
		return nil, fmt.Errorf("load admin credentials: %w", err)
	}
	p, err := c.Parse()
	if err != nil {
		return nil, fmt.Errorf("parse admin credentials: %w", err)
	}
	return p.TLS(), nil
}

// raftAdvertise is the raft address other nodes dial: --raft-advertise, or
// the bound raft address.
func (s *Server) raftAdvertise() string {
	if s.cfg.RaftAdvertiseAddr != "" {
		return s.cfg.RaftAdvertiseAddr
	}
	return string(s.raft.RaftAddr())
}
