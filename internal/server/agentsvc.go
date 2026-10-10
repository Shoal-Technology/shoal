package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/objects"
	"example.com/shoal/internal/security"
	"example.com/shoal/internal/store"
)

// maxStatusMessage bounds what an agent can make the server store per
// replica.
const maxStatusMessage = 1024

type agentService struct {
	v1alpha1.UnimplementedAgentServer
	s *Server
}

// node returns the calling node, which must still be registered: deleting
// the Node object locks the node out until it re-joins.
func (a *agentService) node(ctx context.Context) (caller, error) {
	c, ok := callerFrom(ctx)
	if !ok {
		return caller{}, status.Error(codes.Unauthenticated, "no caller identity")
	}
	if _, err := a.s.store.Get(ctx, v1alpha1.KindNode, c.id.Name); errors.Is(err, store.ErrNotFound) {
		return caller{}, status.Errorf(codes.PermissionDenied, "node %s is not registered (it was deleted); re-join with a new token", c.id.Name)
	} else if err != nil {
		return caller{}, apiError(err)
	}
	return c, nil
}

func (a *agentService) Sync(stream v1alpha1.Agent_SyncServer) error {
	ctx := stream.Context()
	c, err := a.node(ctx)
	if err != nil {
		return err
	}
	name, addr := c.id.Name, peerAddr(ctx)
	a.s.acks.connect(name)
	defer a.s.acks.disconnect(name)
	if err := stream.Send(&v1alpha1.ServerMessage{Msg: &v1alpha1.ServerMessage_Config{Config: &v1alpha1.SyncConfig{
		HeartbeatInterval: durationpb.New(a.s.cfg.HeartbeatInterval),
	}}}); err != nil {
		return err
	}
	a.s.log.Debug("node connected", "node", name, "peer", addr)

	// Receiving and sending each get a goroutine; the first error ends the
	// stream. Only pushAssignments sends after the config above.
	errc := make(chan error, 2)
	go func() {
		reported := map[string]*v1alpha1.ReplicaStatus{} // last status stored per replica
		for {
			msg, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			hb := msg.GetHeartbeat()
			if hb == nil {
				continue
			}
			if err := a.s.nodes.heartbeat(ctx, name, addr, hb); err != nil {
				errc <- apiError(err)
				return
			}
			if err := a.storeReports(ctx, name, hb.GetReplicas(), reported); err != nil {
				errc <- apiError(err)
				return
			}
		}
	}()
	logReqs := a.s.logs.attach(name)
	defer a.s.logs.detach(name, logReqs)
	go func() { errc <- a.pushAssignments(ctx, name, stream, logReqs) }()

	// End the stream when the certificate expires, so the agent reconnects
	// with its renewed one.
	expiry := time.NewTimer(a.s.untilExpiry(c.cert))
	defer expiry.Stop()
	select {
	case err := <-errc:
		if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-expiry.C:
		return status.Error(codes.Unauthenticated, "client certificate expired")
	case <-a.s.shutdown:
		return status.Error(codes.Unavailable, "server shutting down")
	}
}

// pushAssignments sends the node its assignment snapshot whenever it
// changes, and every AssignmentResync even if it didn't (level-triggered: a
// lost or misapplied snapshot is corrected by the next one). It also
// forwards log requests: it is the stream's only sender.
func (a *agentService) pushAssignments(ctx context.Context, node string, stream v1alpha1.Agent_SyncServer, logReqs <-chan *v1alpha1.LogRequest) error {
	// Snapshots depend on replicas, the data they reference, Services and the mesh.
	events := a.s.store.Watch(ctx, v1alpha1.KindReplica, v1alpha1.KindSecret, v1alpha1.KindConfig, v1alpha1.KindService, v1alpha1.KindNode, v1alpha1.KindRoute, v1alpha1.KindCertificate, v1alpha1.KindAcmeChallenge)
	resync := time.NewTicker(a.s.cfg.AssignmentResync)
	defer resync.Stop()
	last, force := "", true
	var routes routePusher
	for {
		snap, err := a.snapshot(ctx, node)
		if err != nil {
			return apiError(err)
		}
		if force || snap.Version != last {
			if err := stream.Send(&v1alpha1.ServerMessage{Msg: &v1alpha1.ServerMessage_Assignments{Assignments: snap}}); err != nil {
				return err
			}
			if snap.Version != last {
				a.s.log.Debug("sent assignments", "node", node, "replicas", len(snap.Assignments), "version", snap.Version)
			}
			last = snap.Version
		}
		// Routes go only to edge nodes; see routeSnapshot.
		rsnap, err := a.s.routeSnapshot(ctx, node)
		if err != nil {
			return apiError(err)
		}
		a.s.acks.sent(node, rsnap.GetEdge(), challengeTokens(rsnap))
		if err := routes.push(stream, rsnap, a.s.now(), force); err != nil {
			return err
		}
		force = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resync.C:
			force = true
		case req := <-logReqs:
			if err := stream.Send(&v1alpha1.ServerMessage{Msg: &v1alpha1.ServerMessage_Logs{Logs: req}}); err != nil {
				return err
			}
			continue
		case _, ok := <-events:
			if !ok {
				return ctx.Err()
			}
			// Coalesce bursts (a scale-up writes several replicas).
			time.Sleep(50 * time.Millisecond)
			for len(events) > 0 {
				<-events
			}
		}
	}
}

// snapshot builds the complete set of replicas bound to node.
func (a *agentService) snapshot(ctx context.Context, node string) (*v1alpha1.AssignmentSnapshot, error) {
	recs, err := a.s.store.List(ctx, v1alpha1.KindReplica)
	if err != nil {
		return nil, err
	}
	data, err := a.s.loadDataSources(ctx)
	if err != nil {
		return nil, err
	}
	snap := &v1alpha1.AssignmentSnapshot{}
	for _, r := range recs {
		rp, err := objects.Replica(r)
		if err != nil {
			return nil, err
		}
		if rp.Spec.GetNode() != node {
			continue
		}
		as := &v1alpha1.Assignment{
			Replica: r.Name, Workload: rp.Spec.GetWorkload(), Template: rp.Spec.GetTemplate(),
			TemplateGeneration: rp.Spec.GetTemplateGeneration(),
		}
		data.resolve(as)
		snap.Assignments = append(snap.Assignments, as)
	}
	if snap.Services, err = a.s.serviceRoutes(ctx); err != nil {
		return nil, err
	}
	if snap.Peers, err = a.s.meshPeers(ctx, node); err != nil {
		return nil, err
	}
	slices.SortFunc(snap.Assignments, func(x, y *v1alpha1.Assignment) int { return cmp.Compare(x.Replica, y.Replica) })
	b, err := objects.Encode(snap)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	snap.Version = hex.EncodeToString(sum[:6])
	return snap, nil
}

// storeReports saves replica statuses reported by node. Only replicas bound
// to that node are accepted, and only changes are written: reported keeps
// what was last stored per replica during this stream.
func (a *agentService) storeReports(ctx context.Context, node string, reports []*v1alpha1.ReplicaReport, reported map[string]*v1alpha1.ReplicaStatus) error {
	for _, rep := range reports {
		id, st := rep.GetReplica(), rep.GetStatus()
		if st == nil {
			continue
		}
		if len(st.Message) > maxStatusMessage {
			st.Message = st.Message[:maxStatusMessage]
		}
		if prev, ok := reported[id]; ok && proto.Equal(prev, st) {
			continue
		}
		rec, err := a.s.store.Get(ctx, v1alpha1.KindReplica, id)
		if errors.Is(err, store.ErrNotFound) {
			continue // removed meanwhile; the next snapshot drops it
		}
		if err != nil {
			return err
		}
		rp, err := objects.Replica(rec)
		if err != nil {
			return err
		}
		if rp.Spec.GetNode() != node {
			a.s.log.Warn("ignoring status for a replica not bound to the node", "node", node, "replica", id)
			continue
		}
		if !proto.Equal(rp.Status, st) {
			b, err := objects.Encode(st)
			if err != nil {
				return err
			}
			if _, err := a.s.store.UpdateStatus(ctx, v1alpha1.KindReplica, id, b); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		reported[id] = st
	}
	return nil
}

func (a *agentService) RenewCertificate(ctx context.Context, req *v1alpha1.RenewCertificateRequest) (*v1alpha1.RenewCertificateResponse, error) {
	c, err := a.node(ctx)
	if err != nil {
		return nil, err
	}
	cert, err := a.s.ca.SignCSR(req.GetCsr(), security.Identity{Name: c.id.Name, Role: security.RoleNode}, a.s.now(), security.NodeValidity)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	a.s.log.Info("node certificate renewed", "node", c.id.Name, "expires", cert.NotAfter)
	return &v1alpha1.RenewCertificateResponse{Certificate: cert.Raw}, nil
}
