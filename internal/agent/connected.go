package agent

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/network"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/security"
	"github.com/Shoal-Technology/shoal/internal/volume"
)

// ConnectedConfig configures an agent that is managed by a server.
type ConnectedConfig struct {
	// Servers are the host:port addresses of the cluster's servers. The agent
	// talks to the leader: when a follower names it, the agent moves there,
	// and it reconnects to the others when the leader is lost. Optional once
	// joined: the saved address is used, and servers given here replace it
	// unless it is one of them.
	Servers []string
	// Token and CAHash are only needed for the first join.
	Token  string
	CAHash string
	// NodeName defaults to the host name.
	NodeName string
	// CredentialsPath is where the node's key and certificates are kept.
	CredentialsPath string
	// AssignmentsPath caches the last assignment snapshot, so the agent
	// keeps running (and restarting) its replicas while the server is
	// unreachable, even across agent restarts.
	AssignmentsPath string
	// Runtime runs the assigned replicas. If nil the agent only registers
	// and sends heartbeats.
	Runtime    runtime.Runtime
	Reconciler Config
	// VolumeRoot is <state-dir>/volumes and VolumeBackend the backend in use
	// (loop or dir). The heartbeat reports both (ADR 0016, section 1.2).
	VolumeRoot, VolumeBackend string
	// SyncInterval is how often the reconciler runs without a new snapshot.
	// Default 2s.
	SyncInterval time.Duration
	// Network, if set, applies Services and mesh peers on the node and
	// serves DNS.
	Network Network
	// Edge, if set, receives the route snapshots the server sends to edge
	// nodes (ADR 0015, pass B2).
	Edge RouteSink
	// WireguardEndpoint overrides the host:port other nodes use to reach
	// this node's mesh (default: the address the server sees).
	WireguardEndpoint string
	// NodeConfigPath caches the node's network settings from the server.
	NodeConfigPath string
	Version        string
	Log            *slog.Logger
	Now            func() time.Time
}

// Network is the node's cluster networking (see package network).
type Network interface {
	Run(ctx context.Context) error
	Sync(ctx context.Context, services []network.Service, peers []network.Peer) error
	WireGuard() (publicKey string, port int)
}

// Connected is an agent managed by a server: it receives assignment
// snapshots, runs them with the reconciler and reports replica statuses in
// its heartbeats.
type Connected struct {
	cfg ConnectedConfig
	log *slog.Logger

	mu        sync.Mutex
	creds     *security.ClientConfig
	ca        *x509.Certificate
	cert      *tls.Certificate // current client certificate (replaced on renewal)
	name      string
	statuses  []*v1alpha1.ReplicaReport // latest reconcile result
	connected bool                      // true while a Sync session is up (see Stats)
	replicas  Stats                     // counts over the latest reconcile result (see Stats)

	reconnect     chan struct{}                     // asks the sync loop to reconnect (after renewal)
	desired       chan *v1alpha1.AssignmentSnapshot // latest snapshot for the reconciler (latest wins)
	statusChanged chan struct{}                     // asks the session to send a heartbeat now

	servers []string    // known servers in order; a server joins the list once it accepts a session
	target  string      // server the next session or request dials (see pickServer)
	hops    int         // leader hints followed since a session last succeeded (see followHint)
	retry   retryPolicy // reconnect backoff (see policy)
}

// Backoff bounds for reconnecting to the server.
const (
	reconnectMin    = time.Second
	reconnectMax    = 30 * time.Second
	reconnectDenied = time.Minute // after the server rejected us
	rpcTimeout      = 30 * time.Second
)

// NewConnected loads the node's saved credentials, or joins the cluster with
// the token if there are none.
func NewConnected(ctx context.Context, cfg ConnectedConfig) (*Connected, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 2 * time.Second
	}
	c := &Connected{
		cfg: cfg, log: cfg.Log,
		reconnect:     make(chan struct{}, 1),
		desired:       make(chan *v1alpha1.AssignmentSnapshot, 1),
		statusChanged: make(chan struct{}, 1),
		servers:       normalizeServers(cfg.Servers),
	}
	if len(c.servers) > 0 {
		c.target = c.servers[0]
	}
	creds, err := security.LoadClientConfig(cfg.CredentialsPath)
	switch {
	case err == nil:
		if err := c.useSaved(creds); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		if err := c.join(ctx); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrPermission):
		return nil, fmt.Errorf("%w (hint: run the agent as root, or choose another --state-dir)", err)
	default:
		return nil, err
	}
	return c, nil
}

func (c *Connected) useSaved(creds *security.ClientConfig) error {
	p, err := creds.Parse()
	if err != nil {
		return fmt.Errorf("%s: %w", c.cfg.CredentialsPath, err)
	}
	id, err := security.IdentityFromCert(p.Cert)
	if err != nil || id.Role != security.RoleNode {
		return fmt.Errorf("%s does not hold node credentials", c.cfg.CredentialsPath)
	}
	if c.cfg.NodeName != "" && c.cfg.NodeName != id.Name {
		return fmt.Errorf("this node already joined as %q; to join as %q, remove %s and join again with a new token",
			id.Name, c.cfg.NodeName, c.cfg.CredentialsPath)
	}
	if !c.cfg.Now().Before(p.Cert.NotAfter) {
		return fmt.Errorf("node certificate expired at %s; remove %s and join again with a new token",
			p.Cert.NotAfter.Format(time.RFC3339), c.cfg.CredentialsPath)
	}
	if c.cfg.Token != "" {
		c.log.Info("already joined; ignoring --token", "node", id.Name)
	}
	if len(c.servers) > 0 && !slices.Contains(c.servers, creds.Server) {
		c.log.Info("server address changed", "old", creds.Server, "new", c.servers[0])
		creds.Server = c.servers[0]
		if err := creds.Save(c.cfg.CredentialsPath); err != nil {
			return err
		}
	}
	if len(c.servers) == 0 {
		c.servers = []string{creds.Server}
	}
	c.creds, c.ca, c.cert, c.name = creds, p.CA, &p.TLSCert, id.Name
	c.target = creds.Server
	return nil
}

func (c *Connected) join(ctx context.Context) error {
	if len(c.servers) == 0 || c.cfg.Token == "" || c.cfg.CAHash == "" {
		return errors.New("not joined yet: --server, --token and --ca-hash are required (see `shoal token create` on the server)")
	}
	name := c.cfg.NodeName
	if name == "" {
		var err error
		if name, err = DefaultNodeName(); err != nil {
			return err
		}
	}
	if !resource.ValidName(name, 63) {
		return fmt.Errorf("node name %q is not a lowercase DNS label; set --node-name", name)
	}
	key, err := security.NewKey()
	if err != nil {
		return err
	}
	csr, err := security.NewCSR(key, name)
	if err != nil {
		return err
	}

	var (
		resp   *v1alpha1.JoinResponse
		addr   string
		caCert *x509.Certificate
	)
	pol := c.policy()
	c.log.Info("joining cluster", "servers", c.servers, "node", name)
	for delay := pol.min; ; delay = min(delay*2, pol.max) {
		addr = c.pickServer()
		var pinned security.PinnedCA
		conn, err := dial(addr, security.PinnedTLS(c.cfg.CAHash, &pinned))
		if err != nil {
			return err
		}
		jctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		resp, err = v1alpha1.NewJoinClient(conn).Join(jctx, &v1alpha1.JoinRequest{Token: c.cfg.Token, NodeName: name, Csr: csr})
		cancel()
		conn.Close()
		if err == nil {
			caCert = pinned.Cert()
			break
		}
		// A rejected token or an untrusted server won't fix itself.
		if perr := pinned.Err(); perr != nil {
			return fmt.Errorf("join %s: %w (check --ca-hash)", addr, perr)
		}
		if leader, ok := leaderHint(err); ok && c.followHint(leader) {
			c.log.Info("server is not the leader; joining through the leader", "server", addr, "leader", leader)
			continue
		}
		if !retryable(err) {
			return fmt.Errorf("join %s: %s", addr, status.Convert(err).Message())
		}
		c.moveOn(addr)
		c.log.Warn("cannot reach server, retrying", "server", addr, "in", delay, "err", err)
		if err := pol.wait(ctx, delay); err != nil {
			return err
		}
	}

	ca, err := x509.ParseCertificate(resp.GetCaCertificate())
	if err != nil || caCert == nil || !ca.Equal(caCert) {
		return errors.New("join: server returned a CA that differs from the pinned one")
	}
	cert, err := checkIssued(resp.GetCertificate(), ca, key)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	creds, err := security.NewClientConfig(addr, ca, cert, key)
	if err != nil {
		return err
	}
	if err := creds.Save(c.cfg.CredentialsPath); err != nil {
		return err
	}
	c.log.Info("joined cluster", "node", name, "server", addr, "credentials", c.cfg.CredentialsPath, "certExpires", cert.NotAfter)
	c.creds, c.ca, c.name = creds, ca, name
	c.cert = &tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
	c.target = addr
	c.servers = addServer(c.servers, addr)
	return nil
}

// DefaultNodeName is the node name used when none is given: the host name
// without its domain, lowercased.
func DefaultNodeName() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("determine node name (use --node-name): %w", err)
	}
	return strings.ToLower(strings.SplitN(h, ".", 2)[0]), nil
}

// checkIssued verifies that a certificate from the server was signed by ca
// and is for key.
func checkIssued(der []byte, ca *x509.Certificate, key crypto.Signer) (*x509.Certificate, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse issued certificate: %w", err)
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return nil, fmt.Errorf("issued certificate not signed by the cluster CA: %w", err)
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(cert.PublicKey) {
		return nil, errors.New("issued certificate is for a different key")
	}
	return cert, nil
}

// Name returns the node's name.
func (c *Connected) Name() string { return c.name }

func (c *Connected) currentCert() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cert, nil
}

// Registry returns the embedded registry at addr as this node reaches it
// (ADR 0020, pass B): the cluster CA it already trusts, and the node's client
// certificate, read again on each handshake so renewal takes effect.
func (c *Connected) Registry(addr string) *runtime.Registry {
	c.mu.Lock()
	ca := c.ca
	c.mu.Unlock()
	return &runtime.Registry{Address: addr, CA: ca, ClientCertificate: c.currentCert}
}

// dial connects to the server the agent currently prefers (see pickServer).
func (c *Connected) dial() (*grpc.ClientConn, error) {
	return c.dialAddr(c.pickServer())
}

func (c *Connected) dialAddr(addr string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	ca := c.ca
	c.mu.Unlock()
	return dial(addr, security.ClientTLS(ca, c.currentCert))
}

func dial(server string, tc *tls.Config) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(server,
		grpc.WithTransportCredentials(credentials.NewTLS(tc)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", server, err)
	}
	return conn, nil
}

// Run keeps a Sync session with the server open, reconnecting with backoff,
// runs the assigned replicas and renews the node certificate, until ctx is
// cancelled. Containers keep running after it returns.
func (c *Connected) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.renewLoop(ctx)
	}()
	if c.cfg.Runtime != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.reconcileLoop(ctx)
		}()
	}
	if c.cfg.Network != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.cfg.Network.Run(ctx); err != nil {
				c.log.Error("node DNS stopped", "err", err)
			}
		}()
	}

	pol := c.policy()
	delay := pol.min
	for {
		addr := c.pickServer()
		start := time.Now()
		err := c.session(ctx, addr)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			delay = pol.min // the session was healthy for a while
		}
		wait := delay
		leader, hinted := leaderHint(err)
		switch code := status.Code(err); {
		case errors.Is(err, errReconnect):
			c.log.Info("reconnecting with renewed certificate")
			wait = 0
		case hinted && c.followHint(leader):
			c.log.Info("server is not the leader; following the leader hint", "server", addr, "leader", leader)
			wait = 0
		case code == codes.PermissionDenied || code == codes.Unauthenticated:
			c.log.Error("server rejected this node", "server", addr, "err", status.Convert(err).Message())
			c.moveOn(addr)
			wait = pol.denied
		default:
			wait = jittered(delay)
			c.log.Warn("lost connection to server", "server", addr, "err", err, "retryIn", wait)
			c.moveOn(addr)
			delay = min(delay*2, pol.max)
		}
		if err := pol.wait(ctx, wait); err != nil {
			return nil
		}
	}
}

var errReconnect = errors.New("reconnect requested")

// session runs one Sync stream with addr until it fails: it hands assignment
// snapshots to the reconciler and sends heartbeats at the interval the
// server asks for, and right away when replica statuses change. A follower
// refuses the stream and names the leader (see Run).
func (c *Connected) session(ctx context.Context, addr string) error {
	conn, err := c.dialAddr(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := v1alpha1.NewAgentClient(conn).Sync(sctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c.served(addr)
	c.setConnected(true)
	defer c.setConnected(false)
	interval := first.GetConfig().GetHeartbeatInterval().AsDuration()
	if interval < time.Second {
		interval = 5 * time.Second
	}
	c.log.Info("connected to server", "server", addr, "node", c.name, "heartbeatInterval", interval)

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if snap := msg.GetAssignments(); snap != nil {
				c.setAssignments(snap)
			}
			if rs := msg.GetRoutes(); rs != nil && c.cfg.Edge != nil {
				if err := c.cfg.Edge.Apply(rs); err != nil {
					c.log.Warn("ignoring route snapshot", "revision", rs.GetRevision(), "err", err)
				} else {
					// Report the applied revision now, not at the next tick:
					// the leader may be waiting on it (ADR 0015, pass E).
					select {
					case c.statusChanged <- struct{}{}:
					default:
					}
				}
			}
			if req := msg.GetLogs(); req != nil {
				go c.uploadLogs(sctx, req)
			}
		}
	}()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := stream.Send(&v1alpha1.AgentMessage{Msg: &v1alpha1.AgentMessage_Heartbeat{Heartbeat: c.heartbeat()}}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-recvErr:
			return err
		case <-c.reconnect:
			return errReconnect
		case <-c.statusChanged:
		case <-tick.C:
		}
	}
}

// NodeConfig returns the node's network settings from the server, which
// assigns the node's container subnet on first use. When the server can't be
// reached it falls back to the last settings received; without those it
// retries until ctx is done (containers can't start without a subnet).
func (c *Connected) NodeConfig(ctx context.Context) (*v1alpha1.NodeConfigResponse, error) {
	pol := c.policy()
	for delay := pol.min; ; delay = min(delay*2, pol.max) {
		addr := c.pickServer()
		cfg, err := c.fetchNodeConfig(ctx, addr)
		if err == nil {
			c.served(addr)
			if b, err := protojson.Marshal(cfg); err == nil && c.cfg.NodeConfigPath != "" {
				if err := security.WriteFileAtomic(c.cfg.NodeConfigPath, b, 0o600); err != nil {
					c.log.Warn("cache node config", "err", err)
				}
			}
			return cfg, nil
		}
		if code := status.Code(err); code == codes.PermissionDenied || code == codes.Unauthenticated {
			return nil, fmt.Errorf("get node config: %s", status.Convert(err).Message())
		}
		if leader, ok := leaderHint(err); ok && c.followHint(leader) {
			c.log.Info("server is not the leader; asking the leader for node config", "server", addr, "leader", leader)
			continue
		}
		c.moveOn(addr)
		if b, rerr := os.ReadFile(c.cfg.NodeConfigPath); rerr == nil {
			cached := &v1alpha1.NodeConfigResponse{}
			if protojson.Unmarshal(b, cached) == nil && cached.GetPodCidr() != "" {
				c.log.Warn("server unreachable, using cached node config", "server", addr, "err", err, "podCIDR", cached.GetPodCidr())
				return cached, nil
			}
		}
		c.log.Warn("cannot get node config from the server, retrying", "server", addr, "in", delay, "err", err)
		if err := pol.wait(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (c *Connected) fetchNodeConfig(ctx context.Context, addr string) (*v1alpha1.NodeConfigResponse, error) {
	conn, err := c.dialAddr(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return v1alpha1.NewAgentClient(conn).NodeConfig(rctx, &v1alpha1.NodeConfigRequest{})
}

// Attach sets the runtime and networking, once the node config is known.
// Call it before Run.
func (c *Connected) Attach(rt runtime.Runtime, n Network) {
	c.cfg.Runtime, c.cfg.Network = rt, n
}

// maxLogChunk bounds the size of one UploadLogs message (gRPC's default
// limit is 4 MB).
const maxLogChunk = 512 << 10

// uploadLogs answers a log request from the server on a separate
// UploadLogs stream. Lines are batched, except when following, where each
// line is sent as it comes.
func (c *Connected) uploadLogs(ctx context.Context, req *v1alpha1.LogRequest) {
	log := c.log.With("replica", req.GetReplica())
	if c.cfg.Runtime == nil {
		return
	}
	conn, err := c.dial()
	if err != nil {
		log.Warn("log upload: connect", "err", err)
		return
	}
	defer conn.Close()
	stream, err := v1alpha1.NewAgentClient(conn).UploadLogs(ctx)
	if err != nil {
		log.Warn("log upload: open stream", "err", err)
		return
	}
	var (
		batch []*v1alpha1.LogLine
		size  int
		first = true
	)
	send := func(errMsg string) error {
		chunk := &v1alpha1.LogChunk{Lines: batch, Error: errMsg}
		if first {
			chunk.RequestId, first = req.GetRequestId(), false
		}
		batch, size = nil, 0
		return stream.Send(chunk)
	}
	readErr := c.cfg.Runtime.Logs(ctx, req.GetReplica(), runtime.LogOptions{Follow: req.GetFollow(), Tail: int(req.GetTail())},
		func(e runtime.LogEntry) error {
			batch = append(batch, &v1alpha1.LogLine{Time: timestamppb.New(e.Time), Stream: e.Stream, Line: e.Line})
			size += len(e.Line) + 64
			if req.GetFollow() || size >= maxLogChunk {
				return send("")
			}
			return nil
		})
	errMsg := ""
	if readErr != nil && ctx.Err() == nil {
		errMsg = readErr.Error()
	}
	if err := send(errMsg); err == nil {
		_, err = stream.CloseAndRecv()
		if err != nil && status.Code(err) != codes.Canceled {
			log.Debug("log upload ended", "err", err)
		}
	}
}

// setAssignments caches a snapshot from the server and hands it to the
// reconciler, replacing one it hasn't picked up yet.
func (c *Connected) setAssignments(snap *v1alpha1.AssignmentSnapshot) {
	if c.cfg.Runtime == nil {
		return
	}
	if c.cfg.AssignmentsPath != "" {
		if b, err := proto.Marshal(snap); err != nil {
			c.log.Error("encode assignments", "err", err)
		} else if err := security.WriteFileAtomic(c.cfg.AssignmentsPath, b, 0o600); err != nil {
			c.log.Error("cache assignments", "err", err) // keep going: the cache only helps restarts
		}
	}
	select {
	case <-c.desired: // drop the stale one
	default:
	}
	c.desired <- snap
}

func (c *Connected) convert(snap *v1alpha1.AssignmentSnapshot) []Assignment {
	out := make([]Assignment, 0, len(snap.GetAssignments()))
	vols := map[string]*v1alpha1.Volume{}
	for _, v := range snap.GetVolumes() {
		vols[v.GetMetadata().GetName()] = v
	}
	for _, a := range snap.GetAssignments() {
		as, err := AssignmentFromAPI(a)
		if err == nil {
			err = bindVolumes(&as, a.GetTemplate(), vols)
		}
		if err != nil {
			c.log.Error("ignoring invalid assignment", "err", err)
			continue
		}
		out = append(out, as)
	}
	return out
}

// loadCachedAssignments returns the last snapshot saved, or false if there
// is none (then the reconciler waits for the server: an empty set would
// remove every container).
func (c *Connected) loadCachedAssignments() (*v1alpha1.AssignmentSnapshot, bool) {
	if c.cfg.AssignmentsPath == "" {
		return nil, false
	}
	b, err := os.ReadFile(c.cfg.AssignmentsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.log.Error("read cached assignments", "err", err)
		}
		return nil, false
	}
	snap := &v1alpha1.AssignmentSnapshot{}
	if err := proto.Unmarshal(b, snap); err != nil {
		c.log.Error("cached assignments are corrupt, waiting for the server", "err", err)
		return nil, false
	}
	c.log.Info("using cached assignments until the server sends new ones", "replicas", len(snap.GetAssignments()))
	return snap, true
}

// reconcileLoop runs the reconciler against the latest snapshot every
// SyncInterval and whenever a new snapshot arrives.
func (c *Connected) reconcileLoop(ctx context.Context) {
	r := NewReconciler(c.cfg.Runtime, c.log, c.cfg.Reconciler)
	defer r.Close()
	jobs := newJobRunner(c.log)
	snap, have := c.loadCachedAssignments()
	desired := c.convert(snap)
	prev := map[string]ReplicaStatus{}
	tick := time.NewTicker(c.cfg.SyncInterval)
	defer tick.Stop()
	for {
		if have {
			c.syncNetwork(ctx, snap)
			r.SetVolumes(snapshotVolumeNames(snap))
			statuses, err := r.Reconcile(ctx, desired)
			if err != nil && ctx.Err() == nil {
				c.log.Warn("reconcile incomplete", "err", err)
			}
			statuses = jobs.run(ctx, c.cfg.Runtime, desired, statuses)
			if ctx.Err() == nil {
				prev = logTransitions(c.log, prev, statuses)
				c.setStatuses(statuses)
			}
		}
		select {
		case <-ctx.Done():
			return
		case snap = <-c.desired:
			desired, have = c.convert(snap), true
		case <-tick.C:
		}
	}
}

// syncNetwork applies the snapshot's Services and mesh peers to the node's
// networking (cheap when nothing changed).
func (c *Connected) syncNetwork(ctx context.Context, snap *v1alpha1.AssignmentSnapshot) {
	if c.cfg.Network == nil {
		return
	}
	var services []network.Service
	for _, s := range snap.GetServices() {
		svc := network.Service{Name: s.GetName(), ClusterIP: s.GetClusterIp(), Endpoints: s.GetEndpoints()}
		for _, p := range s.GetPorts() {
			svc.Ports = append(svc.Ports, network.ServicePort{Port: int(p.GetPort()), TargetPort: int(p.GetTargetPort()), Protocol: p.GetProtocol()})
		}
		services = append(services, svc)
	}
	var peers []network.Peer
	for _, p := range snap.GetPeers() {
		peers = append(peers, network.Peer{Node: p.GetNode(), PublicKey: p.GetPublicKey(), Endpoint: p.GetEndpoint(), PodCIDR: p.GetPodCidr()})
	}
	if err := c.cfg.Network.Sync(ctx, services, peers); err != nil && ctx.Err() == nil {
		c.log.Warn("network sync incomplete", "err", err)
	}
}

// setStatuses records the latest replica statuses for the heartbeat and
// asks for an immediate heartbeat if they changed.
func (c *Connected) setStatuses(statuses []ReplicaStatus) {
	reports := make([]*v1alpha1.ReplicaReport, 0, len(statuses))
	for _, s := range statuses {
		reports = append(reports, &v1alpha1.ReplicaReport{Replica: s.ReplicaID, Status: s.API()})
	}
	c.mu.Lock()
	changed := len(reports) != len(c.statuses)
	for i := 0; !changed && i < len(reports); i++ {
		changed = !proto.Equal(reports[i], c.statuses[i])
	}
	c.statuses = reports
	c.replicas = countReplicas(statuses)
	c.mu.Unlock()
	if changed {
		select {
		case c.statusChanged <- struct{}{}:
		default:
		}
	}
}

func (c *Connected) heartbeat() *v1alpha1.Heartbeat {
	c.mu.Lock()
	reports := c.statuses
	c.mu.Unlock()
	hb := &v1alpha1.Heartbeat{
		Capacity: &v1alpha1.NodeCapacity{
			CpuMillis: int64(goruntime.NumCPU()) * 1000, MemoryBytes: memTotal(), DiskBytes: volume.DiskBytes(c.cfg.VolumeRoot),
		},
		AgentVersion:  c.cfg.Version,
		Os:            goruntime.GOOS,
		Arch:          goruntime.GOARCH,
		Replicas:      reports,
		VolumeBackend: c.cfg.VolumeBackend,
	}
	if c.cfg.Network != nil {
		key, port := c.cfg.Network.WireGuard()
		hb.WireguardPublicKey, hb.WireguardPort, hb.WireguardEndpoint = key, int32(port), c.cfg.WireguardEndpoint
	}
	if c.cfg.Edge != nil {
		hb.RoutesRevision = c.cfg.Edge.AppliedRevision()
	}
	return hb
}

// renewLoop renews the node certificate at two thirds of its lifetime.
func (c *Connected) renewLoop(ctx context.Context) {
	for {
		c.mu.Lock()
		leaf := c.cert.Leaf
		c.mu.Unlock()
		at := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
		if err := sleep(ctx, at.Sub(c.cfg.Now())); err != nil {
			return
		}
		if err := c.renew(ctx); err != nil {
			c.log.Error("certificate renewal failed, retrying in 1m", "err", err, "expires", leaf.NotAfter)
			if err := sleep(ctx, time.Minute); err != nil {
				return
			}
			continue
		}
		select {
		case c.reconnect <- struct{}{}:
		default:
		}
	}
}

func (c *Connected) renew(ctx context.Context) error {
	key, err := security.NewKey()
	if err != nil {
		return err
	}
	csr, err := security.NewCSR(key, c.name)
	if err != nil {
		return err
	}
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := v1alpha1.NewAgentClient(conn).RenewCertificate(rctx, &v1alpha1.RenewCertificateRequest{Csr: csr})
	if err != nil {
		return err
	}
	cert, err := checkIssued(resp.GetCertificate(), c.ca, key)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	creds, err := security.NewClientConfig(c.creds.Server, c.ca, cert, key)
	if err != nil {
		return err
	}
	// Save first: if that fails we keep using the old (still valid) one.
	if err := creds.Save(c.cfg.CredentialsPath); err != nil {
		return err
	}
	c.creds = creds
	c.cert = &tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
	c.log.Info("node certificate renewed", "expires", cert.NotAfter)
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// memTotal returns the machine's memory in bytes, or 0 if unknown (only
// Linux is supported).
func memTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			if err == nil {
				return kb * 1024
			}
		}
	}
	return 0
}
