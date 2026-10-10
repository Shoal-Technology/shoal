// Package server implements the control plane: the gRPC API (Control, Join,
// Agent), backed by the state store, plus node heartbeat tracking.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"example.com/shoal/api/v1alpha1"
	"example.com/shoal/internal/controller"
	"example.com/shoal/internal/security"
	"example.com/shoal/internal/store"
	"example.com/shoal/internal/store/raftstore"
	"example.com/shoal/internal/store/sqlite"
)

// Config configures the server. Zero durations get defaults.
type Config struct {
	DataDir string
	// AdvertiseAddress is the host:port clients use to reach the server; it
	// is written to admin.conf.
	AdvertiseAddress string
	Log              *slog.Logger
	Now              func() time.Time

	// EncryptionKeyFile holds the key that encrypts Secrets at rest;
	// default <data-dir>/secret.key, created on first start.
	EncryptionKeyFile string
	// ClusterCIDR is split into one /24 container subnet per node; default
	// 10.250.0.0/16. ServiceCIDR holds Service virtual IPs; default
	// 10.96.0.0/16.
	ClusterCIDR string
	ServiceCIDR string

	HeartbeatInterval      time.Duration // asked of agents, default 5s
	NodeNotReadyAfter      time.Duration // default 20s
	NodeStatusPersistEvery time.Duration // default 30s
	NodeCheckInterval      time.Duration // default 2s
	ShutdownTimeout        time.Duration // default 10s
	AssignmentResync       time.Duration // resend unchanged snapshots, default 30s
	EvictAfter             time.Duration // replace replicas of silent nodes, default 60s
	ControllerInterval     time.Duration // periodic controller passes, default 5s

	// StoreBackend selects the state store: StoreSQLite (default) or
	// StoreRaft. With raft the node is the only voter; see ADR 0014.
	StoreBackend string
	// RaftBindAddr is where Raft listens (StoreRaft only); default 127.0.0.1:7445.
	RaftBindAddr string
	// RaftJoinAddr is the API address of an existing server. Set, the node
	// joins that cluster instead of bootstrapping one (StoreRaft only).
	RaftJoinAddr string
	// RaftAdvertiseAddr is the raft address other nodes dial; default is the
	// bound raft address (StoreRaft only).
	RaftAdvertiseAddr string
	// RaftNodeID names this node in the Raft cluster (StoreRaft only);
	// default is the host name.
	RaftNodeID string

	// leaderGate is a test hook that records which nodes run the leader
	// tasks. Nil in production.
	leaderGate *leaderGate
}

// Store backends for Config.StoreBackend.
const (
	StoreSQLite = "sqlite"
	StoreRaft   = "raft"
)

// raftReadyTimeout bounds the wait for the raft store at startup. A node with
// no leader fails New after this long instead of hanging.
const raftReadyTimeout = 30 * time.Second

func (c *Config) setDefaults() {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.HeartbeatInterval, 5*time.Second)
	def(&c.NodeNotReadyAfter, 20*time.Second)
	def(&c.NodeStatusPersistEvery, 30*time.Second)
	def(&c.NodeCheckInterval, 2*time.Second)
	def(&c.ShutdownTimeout, 10*time.Second)
	def(&c.AssignmentResync, 30*time.Second)
	def(&c.EvictAfter, 60*time.Second)
	def(&c.ControllerInterval, 5*time.Second)
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.EncryptionKeyFile == "" {
		c.EncryptionKeyFile = filepath.Join(c.DataDir, "secret.key")
	}
	if c.ClusterCIDR == "" {
		c.ClusterCIDR = "10.250.0.0/16"
	}
	if c.ServiceCIDR == "" {
		c.ServiceCIDR = "10.96.0.0/16"
	}
	if c.StoreBackend == "" {
		c.StoreBackend = StoreSQLite
	}
	if c.RaftBindAddr == "" {
		c.RaftBindAddr = "127.0.0.1:7445"
	}
	if c.RaftNodeID == "" {
		c.RaftNodeID, _ = os.Hostname()
	}
}

// Server is the control plane.
type Server struct {
	cfg    Config
	log    *slog.Logger
	store  store.Store
	ca     *security.CA
	nodes  *nodeTracker
	ctrl   *controller.Controller
	logs   *logBroker
	cidrs  *cidrAllocator
	portal *portal
	grpc   *grpc.Server

	// raft is the raft store (StoreRaft only), nil otherwise.
	raft *raftstore.Store

	// gate is the test hook from Config.leaderGate; nil in production.
	gate *leaderGate

	// leadership carries this node's leadership changes. Background writers
	// run only while it reports true.
	leadership <-chan bool

	shutdown     chan struct{} // closed when Serve starts shutting down
	shutdownOnce sync.Once
}

// AdminConfigPath is where the server writes the CLI's credentials.
func AdminConfigPath(dataDir string) string { return filepath.Join(dataDir, "admin.conf") }

// New opens the state store, loads (or creates) the cluster PKI and prepares
// the gRPC server. Call Serve, then Close.
func New(ctx context.Context, cfg Config) (*Server, error) {
	cfg.setDefaults()
	if cfg.DataDir == "" || cfg.AdvertiseAddress == "" || cfg.Log == nil {
		return nil, errors.New("server: DataDir, AdvertiseAddress and Log are required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	now := cfg.Now()
	ca, created, err := security.LoadOrCreateCA(filepath.Join(cfg.DataDir, "pki"), now)
	if err != nil {
		return nil, err
	}
	if created {
		cfg.Log.Info("generated cluster CA", "caHash", security.CAHash(ca.Cert))
	}
	// The server certificate is cheap to issue and never stored, so it is
	// fresh on every start.
	key, err := security.NewKey()
	if err != nil {
		return nil, err
	}
	leaf, err := ca.IssueServer(key.Public(), now)
	if err != nil {
		return nil, err
	}
	cidrs, err := newCIDRAllocator(cfg.ClusterCIDR, cfg.ServiceCIDR)
	if err != nil {
		return nil, err
	}
	encKey, created, err := security.LoadOrCreateKey(cfg.EncryptionKeyFile)
	if err != nil {
		return nil, err
	}
	if created {
		cfg.Log.Info("generated secret encryption key", "path", cfg.EncryptionKeyFile)
	}
	var inner store.Store
	var leadership <-chan bool
	var raftStore *raftstore.Store
	switch cfg.StoreBackend {
	case StoreSQLite:
		db, err := sqlite.Open(ctx, filepath.Join(cfg.DataDir, "state.db"), cfg.Now)
		if err != nil {
			return nil, err
		}
		inner = db
		leadership = alwaysLeader()
	case StoreRaft:
		if cfg.RaftNodeID == "" {
			return nil, errors.New("server: RaftNodeID is required with the raft store")
		}
		rs, err := raftstore.Open(raftstore.DiskConfig{
			DataDir:  filepath.Join(cfg.DataDir, "raft"),
			NodeID:   raft.ServerID(cfg.RaftNodeID),
			BindAddr: cfg.RaftBindAddr,
			Join:     cfg.RaftJoinAddr != "",
		})
		if err != nil {
			return nil, err
		}
		// WaitReady runs later, once the admin credentials exist and any
		// join has been made (see below).
		raftStore = rs
		inner = rs
		leadership = rs.LeaderCh()
	default:
		return nil, fmt.Errorf("unknown store backend %q (want %q or %q)", cfg.StoreBackend, StoreSQLite, StoreRaft)
	}
	// Everything goes through the encrypting store, so Secrets are only
	// ever written encrypted.
	st, err := store.NewEncryptingStore(inner, encKey, v1alpha1.KindSecret)
	if err != nil {
		inner.Close()
		return nil, err
	}

	s := &Server{
		cidrs: cidrs,
		cfg:   cfg,
		log:   cfg.Log,
		store: st,
		raft:  raftStore,
		gate:  cfg.leaderGate,
		ca:    ca,
		nodes: newNodeTracker(st, cfg.Log, cfg.Now, cfg.NodeNotReadyAfter, cfg.NodeStatusPersistEvery),
		ctrl: controller.New(controller.Config{
			Store: st, Log: cfg.Log, Now: cfg.Now, EvictAfter: cfg.EvictAfter, Interval: cfg.ControllerInterval,
			ServiceCIDR: cidrs.services,
		}),
		logs:       newLogBroker(),
		leadership: leadership,
		shutdown:   make(chan struct{}),
	}
	if err := s.ensureAdminConfig(); err != nil {
		st.Close()
		return nil, err
	}
	var joinIdx uint64
	if raftStore != nil {
		// A joining server asks the cluster to add it before it waits for
		// the log: until then no leader is known to it.
		if cfg.RaftJoinAddr != "" {
			joinIdx, err = s.joinCluster(ctx)
			if err != nil {
				st.Close()
				return nil, err
			}
		}
		// Apply up to the leader's commit index at the join. Until then the
		// store may miss the bootstrap records written before it.
		if joinIdx > 0 {
			if err := raftStore.WaitApplied(ctx, joinIdx); err != nil {
				st.Close()
				return nil, fmt.Errorf("joined raft cluster but log not applied: %w", err)
			}
		}
		// The wait is bounded: with no leader it fails with an error instead
		// of blocking forever. Nothing may read the store or start
		// controllers before the log replayed on restart has been applied.
		wctx, cancel := context.WithTimeout(ctx, raftReadyTimeout)
		err = raftStore.WaitReady(wctx)
		cancel()
		if err != nil {
			st.Close()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("raft store not ready after %s: no leader or log not caught up (see ADR 0014)", raftReadyTimeout)
			}
			return nil, fmt.Errorf("raft store not ready: %w", err)
		}
	}
	if err := s.ensureUIPassword(ctx); err != nil {
		st.Close()
		return nil, err
	}
	s.grpc = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(security.ServerTLS(ca.Cert, leaf, key))),
		grpc.ChainUnaryInterceptor(s.unaryAuth, s.unaryLeader),
		grpc.ChainStreamInterceptor(s.streamAuth),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: time.Minute, Timeout: 20 * time.Second}),
	)
	v1alpha1.RegisterControlServer(s.grpc, &controlService{s: s})
	v1alpha1.RegisterJoinServer(s.grpc, &joinService{s: s})
	v1alpha1.RegisterAgentServer(s.grpc, &agentService{s: s})
	return s, nil
}

func (s *Server) now() time.Time { return s.cfg.Now() }

// AdvertiseAddress is the host:port clients use to reach the server.
func (s *Server) AdvertiseAddress() string { return s.cfg.AdvertiseAddress }

// CAHash returns the hash agents pin to trust this server.
func (s *Server) CAHash() string { return security.CAHash(s.ca.Cert) }

// ensureAdminConfig writes admin.conf unless a valid one for this CA and
// address exists that is good for at least another 30 days.
func (s *Server) ensureAdminConfig() error {
	path := AdminConfigPath(s.cfg.DataDir)
	if c, err := security.LoadClientConfig(path); err == nil {
		if p, err := c.Parse(); err == nil && p.CA.Equal(s.ca.Cert) && c.Server == s.cfg.AdvertiseAddress &&
			p.Cert.NotAfter.After(s.now().Add(30*24*time.Hour)) {
			return nil
		}
	}
	key, err := security.NewKey()
	if err != nil {
		return err
	}
	cert, err := s.ca.IssueClient(key.Public(), security.Identity{Name: "admin", Role: security.RoleAdmin}, s.now(), security.AdminValidity)
	if err != nil {
		return err
	}
	c, err := security.NewClientConfig(s.cfg.AdvertiseAddress, s.ca.Cert, cert, key)
	if err != nil {
		return err
	}
	if err := c.Save(path); err != nil {
		return err
	}
	s.log.Info("wrote admin credentials", "path", path, "server", s.cfg.AdvertiseAddress, "expires", cert.NotAfter)
	return nil
}

// LocalNodeCredentials registers node name and returns credentials for an
// agent in the same process (`shoal server --with-agent`), so single-node
// setups need no join token. The agent renews them through the API as usual.
func (s *Server) LocalNodeCredentials(ctx context.Context, name string) (*security.ClientConfig, error) {
	key, err := security.NewKey()
	if err != nil {
		return nil, err
	}
	cert, err := s.ca.IssueClient(key.Public(), security.Identity{Name: name, Role: security.RoleNode}, s.now(), security.NodeValidity)
	if err != nil {
		return nil, err
	}
	st, err := encode(&v1alpha1.NodeStatus{Phase: NodeNotReady})
	if err != nil {
		return nil, err
	}
	if _, err := s.store.Create(ctx, store.Record{Kind: v1alpha1.KindNode, Name: name, Status: st}); err != nil && !errors.Is(err, store.ErrExists) {
		return nil, fmt.Errorf("register local node %s: %w", name, err)
	}
	s.log.Info("registered local node", "node", name)
	return security.NewClientConfig(s.cfg.AdvertiseAddress, s.ca.Cert, cert, key)
}

// Serve accepts connections on lis until ctx is cancelled, then shuts down
// gracefully: agent streams are closed (agents reconnect to the next server)
// and in-flight calls get ShutdownTimeout to finish.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	bg, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runWhileLeader(bg, s.leadership, s.runLeaderTasks)
	}()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		s.shutdownOnce.Do(func() { close(s.shutdown) })
		done := make(chan struct{})
		go func() { s.grpc.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(s.cfg.ShutdownTimeout):
			s.log.Warn("graceful shutdown timed out, closing remaining connections")
			s.grpc.Stop()
		}
	}()

	s.log.Info("server listening", "address", lis.Addr().String(), "advertise", s.cfg.AdvertiseAddress)
	err := s.grpc.Serve(lis)
	cancel()
	if ctx.Err() != nil {
		<-stopped
		err = nil // the expected result of GracefulStop/Stop
	}
	wg.Wait()
	return err
}

// Close releases the store. Call it after Serve returns.
func (s *Server) Close() error { return s.store.Close() }

// runLeaderTasks runs the background writers: node status, controllers and
// join token cleanup. It runs only while this node is the leader (see
// runWhileLeader) and returns when ctx is cancelled.
func (s *Server) runLeaderTasks(ctx context.Context) {
	s.gate.enter(s.cfg.RaftNodeID)
	defer s.gate.leave(s.cfg.RaftNodeID)
	// A new leader must not judge nodes by heartbeats it never saw.
	s.nodes.beginTerm(s.now())
	// Followers find the leader's API through this record, so it must be
	// written before anything else the leader does.
	for {
		err := s.publishLeader(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Error("publish leader record failed, retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		s.nodes.run(ctx, s.cfg.NodeCheckInterval)
	}()
	go func() {
		defer wg.Done()
		s.ctrl.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		s.runTokenGC(ctx, time.Hour)
	}()
	wg.Wait()
}

func (s *Server) runTokenGC(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := s.gcTokens(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("join token cleanup failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
