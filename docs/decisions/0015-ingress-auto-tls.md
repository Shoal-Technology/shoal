# 0015: Ingress and automatic TLS

Status: Accepted (Phase 6 follow-up). Pass A (Route object) is implemented. Pass B1 (edge proxy core in internal/edge: routing table, proxy, TLS helpers) is implemented. Pass B2 (route delivery to edge agents, the edge listeners on agents, RouteStatus, `shoal label`; no ACME) is implemented. Pass B2 leaves out the on-disk route cache and the real-host check, listed under Pass B2 below. Pass C1 (ACME issuance and renewal on the leader, HTTP-01 challenges on edges) is implemented; see Pass C1 below. Pass D (pebble in CI) and E are not implemented.

## Context
Workloads are reachable inside the cluster through Services (ADR 0012): a
virtual IP, `<name>.shoal` DNS, and iptables load balancing on every node.
Nothing yet brings HTTP traffic from outside the cluster to a Service. Users
today either publish host ports (`hostPort`, one node, no TLS) or put their
own reverse proxy in front. Shoal should offer a small, built-in HTTP
ingress: host and path routing to a Service, TLS certificates obtained and
renewed automatically, and no extra component to install.

The design has to fit the existing shape of the system:
- Agents connect outbound to the server over gRPC with mTLS (ADR 0003). The
  server never dials a node, so any inbound listener must live on the node
  and receive its config through the existing agent session.
- Object kinds are protobuf messages in `api/v1alpha1/resources.proto`,
  decoded in `internal/objects`, validated in `internal/resource`
  (`kinds.go`, `workload.go`) and applied through `internal/server/objects.go`.
- Secrets are encrypted by `store.EncryptingStore` (AES-256-GCM, key from
  `--encryption-key-file`, AAD = kind/name) for the kinds listed in
  `server.go` (today only `Secret`).
- The control plane is either sqlite (single node) or the Raft store (ADR
  0014). Controllers run only while the node is leader, using the
  `LeaderCh()` pattern.
- The service proxy and DNS are in `internal/network` and run on every
  agent. Service endpoints (`Service.status.endpoints`) are already
  computed by the controller and delivered in the assignment snapshot.

## Decision

### 1. Object model: `Route`
A new kind `Route`. One Route maps one host and optional path prefix to one
Service port. The name is `Route` rather than `Ingress` to avoid confusion
with the Kubernetes object of the same name.

```yaml
kind: Route
metadata:
  name: web
spec:
  host: app.example.com       # required, lowercase DNS name, no IP, no wildcard in v1
  pathPrefix: /               # optional, default "/", must start with "/"
  service: web                # Service name (names are cluster-wide)
  port: 80                    # Service port (not the target port)
  tls: auto                   # auto | none | secret
  # tlsSecret: web-cert       # only with tls: secret; a Secret holding PEM cert + key
  redirectHttp: true          # port 80 answers 308 to https; default true when tls != none
```

Proto: `RouteSpec { string host; string path_prefix; string service; int32
port; string tls; string tls_secret; optional bool redirect_http; }` (tls is a
string in pass A, see Decisions). Default
`tls` is `auto`. `RouteStatus { string state; string message; string
cert_state; }`, where `state` is `Accepted` or `Rejected` (for example
Service missing, duplicate host and path) and `cert_state` mirrors the
Certificate record (section 3).

Validation (`internal/resource`, same place as Service validation):
- host is a valid DNS name, lowercased, at most 253 characters, not an IP;
- pathPrefix starts with `/`, has no query, at most 256 characters;
- `(host, pathPrefix)` is unique across Routes. Duplicate routes are
  Rejected, not silently merged. Ties on one host go to the longest prefix;
- `tls: secret` requires `tlsSecret`, and the Secret must exist when the
  route is accepted;
- `tls: none` forbids `redirectHttp: true`.

Apply: `shoal apply -f` accepts `Route` in `objects.go` next to Workload,
Service, Secret and Config. CLI and portal gain list, get and delete through
the same paths.

### 2. Edge proxy: labelled nodes, not the servers
The edge runs in the agent, on nodes labelled `shoal.dev/edge=true`. An
operator sets the label with `shoal label node <name> shoal.dev/edge=true`,
which stores it in `NodeSpec.labels`. The server sends route config only to
labelled nodes, and an agent binds its listeners (`--edge-http`, default
`:80`, and `--edge-https`, default `:443`; an empty value disables one) only
after a route snapshot says the node is an edge. Nodes without the label bind
no ports and never receive a route snapshot.

Why agents and not servers:
- agents are the nodes with container bridges, and the proxy needs to reach
  container IPs without a VIP hop;
- servers hold the control plane; exposing 80/443 there mixes concerns and
  makes a public HTTP flood hit Raft;
- agents already accept config through the session, so no new inbound path
  to the server is needed.

Operators point DNS at the edge nodes (two or more for availability). DNS is
external and out of scope; Shoal does not manage A records.

Reaching endpoints: the edge does not use the iptables VIP. It takes the
Service's `status.endpoints` (ready replicas only, from the controller) and
dials `endpointIP:targetPort` directly, round robin over the endpoints. Cross-node
traffic goes over the WireGuard mesh routes that already exist (ADR 0012);
same-node traffic goes over the bridge. Pass B verifies that the host can
route to container subnets from the host namespace.

Route config reaches the edge through the existing agent session. The server
adds a `RouteSnapshot` to the same stream that carries `AssignmentSnapshot`
(`internal/agent/connected.go`). It contains the Routes, their target
endpoints, and certificate bundles for edge nodes only. Like the assignment
snapshot, the agent caches the last snapshot on disk, so the edge keeps
serving during a server outage. The snapshot is versioned by store revision,
and the edge applies only newer ones.

Health-aware selection: the controller only lists Ready endpoints, which
handles most cases. On top of that the edge does passive checks: a dial
failure or a 502/503/504 from an endpoint puts it in a 10-second cooldown,
and the request is retried once on another endpoint. Active HTTP probes are
out of scope for v1.

HTTP proxy: `net/http/httputil.ReverseProxy` with a custom transport. It
handles websockets (connection upgrade) without extra code. Inbound HTTP/1.1
and HTTP/2 are served by `net/http` (HTTP/2 over TLS through the standard
ALPN support; cleartext h2c is not in v1). Upstream is HTTP/1.1 only in v1.

Timeouts (defaults, configurable per agent later): read-header 10s, idle
120s, upstream dial 5s, upstream response-header 60s. No whole-request write
timeout, so streaming responses and websockets keep working.

Headers: the edge sets `X-Forwarded-For` to the client address it sees
(overwriting any client-supplied value), `X-Forwarded-Proto` and
`X-Forwarded-Host` from the connection. It does not trust upstream proxies in
v1; a load balancer in front of the edge is not supported until PROXY
protocol is added (open question).

Access logs: one structured JSON line per request through the agent's
`slog` logger, with host, route name, method, path (without query), status,
bytes, duration and upstream endpoint. Query strings are not logged because
they often carry tokens.

### 3. ACME and certificates
Client: the lower-level `golang.org/x/crypto/acme` client, not `autocert`.
`autocert` owns its own cache, challenge handling and renewal loop per
process. That fits one process per domain, not one leader ordering for
several edges that share challenge tokens through the store. The lower-level
client gives control over which node orders, where tokens are stored, and when
the leader accepts a challenge.

Account: the leader creates one ECDSA P-256 account key per ACME directory
(record kind `AcmeAccount`, keyed by directory URL). The key is encrypted at
rest by the same mechanism as Secrets. Changing the directory (staging or
production) creates a new account and does not reuse the old key.

Staging: `shoal server --acme-staging` selects the Let's Encrypt staging
directory. The default is production. Staging certificates are not trusted by
browsers, so the Route status shows the issuer and whether it is staging.

Certificate records: kind `Certificate`, name = host, containing the PEM chain,
the private key, not-before and not-after, the ACME order URL and the
renewal state. The private key is stored encrypted. The `EncryptingStore`
kind list grows from `Secret` to `Secret, Certificate, AcmeAccount,
AcmeChallenge`. Raft snapshots and backups then carry only ciphertext. Keys
are never written to plain files on servers.

Only Routes with `tls: auto` produce a Certificate. A Certificate is never
issued for a host without a Route, so a client cannot make Shoal request
certificates for arbitrary names.

HTTP-01 challenge with several edges: the leader writes one `AcmeChallenge`
record per token (name = token, value = key authorization, expiry 10
minutes). The server includes the active challenges in `RouteSnapshot`, and
every edge answers `GET /.well-known/acme-challenge/<token>` from its local
copy. Validation can reach any edge, so the leader does not call `Accept`
until every connected edge has acknowledged the snapshot revision that
includes the token (acks ride on the existing heartbeat). If an edge
disconnects during validation, the order is retried. The challenge path is
always served on port 80 before any redirect.

Leader gating: an `ingress` controller runs only while `LeaderCh()` reports
this node as leader, like the other controllers (ADR 0014). It is the only
writer of Certificate and AcmeAccount records. Orders are restartable: the
order URL is stored in the Certificate record, so a new leader resumes an
in-flight order or starts a new one after its token expires.

Renewal: check every 12 hours with up to one hour of jitter. Renew when less
than 30 days remain on a 90-day certificate. On failure, retry with
exponential backoff from 1 hour, capped at 24 hours. Renewal is not blocked
by the Route being edited, only by the Route existing with `tls: auto`.

Rate limits: the controller reads the directory's limits and tracks them in
the Certificate status. A `rateLimited` error sets `retryAfter` and suppresses
new orders for that host until it passes. The relevant limits (verify the
current numbers before pass C) are duplicate certificates per week, failed
validations per hostname per hour, and new orders per account per window.

Hot load: edges store the current `tls.Certificate` values in an atomic
pointer. `tls.Config.GetCertificate` looks up the SNI name there. A new
`RouteSnapshot` swaps the pointer without restarting the listener or dropping
connections. Existing connections keep the old certificate until they close.

Before a certificate exists, a `tls: auto` route has no certificate. The edge
fails the TLS handshake for that name, and the Route status says
`cert_state: Pending`.

### 4. HTTP features in scope for v1
In scope:
- host and path-prefix routing (longest prefix wins, then route order);
- websockets (via ReverseProxy upgrade);
- HTTP/2 over TLS;
- timeouts as above;
- `X-Forwarded-*` as above;
- round-robin over Ready endpoints, with passive cooldown;
- access logs.

Out of scope, recorded as later work:
- rate limiting and connection limits (later pass, per Route);
- authentication or authorization at the edge (forward auth, OIDC);
- canary or weighted routing across Services;
- header rewriting and redirect rules beyond `redirectHttp`;
- active health probes;
- upstream TLS and h2c;
- PROXY protocol and trusted forwarders;
- wildcard hosts, and DNS-01 challenges (needed for wildcard certificates
  and for hosts where HTTP-01 cannot reach an edge);
- TCP and UDP ingress.

### 5. Security
- SNI: the TLS listener accepts only names that have a certificate in the
  edge's current snapshot. Unknown or missing SNI fails the handshake; it
  does not present a default certificate.
- Unknown host on HTTP or HTTPS: the edge returns a fixed `404 Not Found`
  with a generic body, the same for every unknown host, with no list of
  routes, no Service names, and no version string. Response timing is not
  equalised; that is accepted.
- Hostnames are case-folded and the port is stripped before lookup. Host
  headers with other shapes get the same 404.
- Certificate keys: encrypted in the store (section 3); in memory on edges
  only; the edge's cached snapshot file is written with 0600 permissions and
  contains the keys only if pass B decides to cache them (open question).
  Keys travel to edges over the existing mTLS agent session, the same trust
  boundary that Secrets already cross today.
- Who may create Routes: not defined yet. Shoal has no RBAC. Any client that
  can apply objects can create or change Routes, so anyone who can apply
  Routes can also point a hostname at any Service and request a certificate
  for it. RBAC is a prerequisite for multi-team clusters and is listed as
  later work. Until then, Route creation is as privileged as Secret creation.
- Route creation does not run a host ownership check. ACME HTTP-01 proves
  control of the name, which is the only check v1 performs.
- Logs never contain query strings, cookies or Authorization headers.

### 6. Testing plan
Unit tests (`go test ./...`, no network):
- `Route` validation: hosts, prefixes, duplicates, tls-mode rules;
- routing table: longest prefix, case folding, port stripping;
- endpoint selection: round robin over Ready endpoints, cooldown and retry;
- certificate selection by SNI, and hot swap via snapshot;
- `EncryptingStore` round trip for the new kinds (store test pattern);
- rate-limit and backoff logic, with a fake clock.

In-process tests with a fake ACME server: pebble
(`github.com/letsencrypt/pebble`), run as a child process from a test with a
build tag (`acme_integration`). Its `challtestsrv` resolves test names to
loopback. The pebble config sets its HTTP-01 validation port to the edge's
test listener port. The test edge binds an ephemeral loopback port, never
80 or 443. The x/crypto/acme package has no fake server of its own, so pebble
is the only option that exercises the real protocol. The fake-server tests
cover: first issuance, renewal at the threshold, multi-edge validation
(two edge processes, the token answered by the one that did not order),
leader failover mid-order, and rate-limit responses (pebble's configurable
limits or an injected error).

CI workflow (`.github/workflows/ingress.yml`): runs on a disposable GitHub
hosted runner, installs pebble and runs the `acme_integration` tests. Nothing
in CI binds 80 or 443 on a shared machine.

Shared VPS: `codex-vps` must never bind 80 or 443 for ingress work. Tests use
only loopback ephemeral ports. `--edge-listen` defaults to empty, so an
ordinary `shoal agent` binds nothing new. Manual testing on the VPS uses
high ports only (for example 8080 and 8443), and the Makefile target for
the edge refuses ports 80 and 443 unless `SHOAL_EDGE_ALLOW_PRIVILEGED=1` is
set on a disposable machine.

### 7. Implementation plan
Each pass is about one agent session and leaves the tree green.

- **Pass A: Route object, validation, CLI.** Proto messages and generated
  code; `Route` in `objects.go` and `internal/resource`; apply, get, list and
  delete through the CLI and API; unique-host check; status `Accepted` or
  `Rejected`. No edge, no ACME. Tests: validation and apply round trips.
- **Pass B: edge proxy with static or self-signed certificates.**
  `RouteSnapshot` in the agent session and cache; `--edge-listen` and the
  edge label; ReverseProxy with endpoint round robin, cooldown, timeouts,
  `X-Forwarded-*`, access logs, redirect and the 404 path; certificates from
  `tls: secret` Secrets, plus a self-signed certificate for local tests.
  Verifies host-to-container-subnet reachability on a two-node setup.
- **Pass C: ACME, certificate store, leader renewal.** `Certificate` and
  `AcmeAccount` kinds added to the encrypted kinds list; the `ingress`
  controller under leader gating; the HTTP-01 flow with `AcmeChallenge`
  records, the snapshot acks and the staging flag; renewal timing, backoff
  and rate-limit tracking; hot cert load on edges. Tests: unit tests, plus
  pebble locally.
- **Pass D: CI with pebble.** `ingress.yml` on a disposable runner; the
  `acme_integration` suite; the VPS guard in the Makefile; docs for the
  staging flag and DNS setup.
- **Pass E: hardening and docs.** Access-log review, timeout and header
  edge cases (websocket close, slow clients), a README section, and a
  two-node manual check on the VPS using high ports only.

Passes A and B can be built before C. Without C, `tls: auto` routes stay
Pending and the edge serves only `tls: none` and `tls: secret` routes.

## Consequences
- Edges are agents, so the ingress IP is whichever edge DNS points at. Two
  or more edges are needed for availability, and DNS failover is the
  operator's job. A load balancer in front is not supported in v1.
- Config and certificates reuse the agent session and the encrypted store.
  No new trust channel and no new inbound port on servers.
- Certificate issuance depends on leader gating and on edge acks. A
  partitioned edge can delay validation; the leader retries rather than
  giving up.
- Let's Encrypt limits are external. A misconfigured Route can hit them, so
  staging must be the default for any test, and the backoff and status
  fields exist to make that visible.
- Route creation is as powerful as Secret creation until RBAC exists.
- Endpoint selection depends on the controller's readiness view, which
  already has the partition caveats in ADR 0012. Passive cooldown reduces
  the window but does not remove it.
- No WAF, auth, rate limits or canary routing in v1. Users who need them
  keep a separate proxy in front, which the edge does not prevent.

## Decisions
Taken when the ADR was accepted. Each one answers an open question below.

1. **Edge keys are held in memory only (v1).** Edges keep no disk cache of
   certificate private keys. The on-disk snapshot cache may hold Routes and
   endpoints, never keys. After an edge restart without a server, its
   `tls: auto` and `tls: secret` routes have no certificate until the server
   sends a snapshot again. This is the cost of not writing keys to disk.
2. **Edge label is `shoal.dev/edge=true`, set by the operator.** The label
   marks a node as an edge and is stored in `NodeSpec.labels`, set only by
   `shoal label node` (the server's Control API). An agent cannot label
   itself, because a self-declared edge would receive every route Secret. A
   server that also runs an agent may carry the label. The `--edge-http` and
   `--edge-https` flags pick the listen addresses (section 2); the label is
   what makes the server send route config to the node.
3. **PROXY protocol and DNS-01 are deferred.** Neither is in v1. A load
   balancer in front of edges is unsupported. Wildcard hosts are rejected at
   validation with a message that says so.
4. **RouteSnapshot rides on the existing agent session.** It is sent on the
   same stream as `AssignmentSnapshot`, not a separate gRPC stream. Pass B
   decides the message framing; if head-of-line blocking shows up in
   practice, a separate stream can be added later without changing Route.

### Pass A rules for Route
- **Wire form of tls:** a string (`auto`, `none`, `secret`), not a protobuf
  enum. The YAML values are lowercase, and protobuf enum names would force
  `TLS_SECRET` in manifests. This matches `restartPolicy` and
  `partitionPolicy`, which are also validated strings.
- **Host:** a lowercase DNS name of at least two labels, at most 253 characters,
  no trailing dot, not an IP address, last label not all digits. Uppercase is
  rejected, not folded, so the stored name is the one users typed. A `*` is
  rejected with a message saying wildcards are not supported in v1.
- **pathPrefix:** defaults to `/`. Must start with `/`, at most 256
  characters, and must not contain `?` or `#`.
- **service and port:** `service` is a Service name; `port` is 1 to 65535.
  The service is **not** checked at apply time. Workload selectors and
  Workload-to-Secret references are not checked at apply either, so Routes
  follow the same rule. The edge reports a missing Service through
  `status` in pass B.
- **tls:** `auto` (default), `none` or `secret`. `secret` requires `tlsSecret`
  naming a Secret; the Secret is not checked at apply time (same reason as
  above). `tlsSecret` is also rejected with `auto` and `none`, so a stray
  field cannot look like it does something.
- **redirectHttp:** defaults to true unless tls is `none`. An explicit true
  with `tls: none` is rejected.
- **Status:** apply stores the spec only, so status is empty after apply. The
  controller writes it from pass B2 on (see Pass B2).

### Conflicts and matching (longest prefix)
- Two Routes with the same host and the same pathPrefix are rejected, whether
  the other one is stored or earlier in the same apply batch. Re-applying a
  Route to itself is fine. The check reads the store and is not atomic with
  the write, so two concurrent applies can both pass. A controller-side
  re-check is the fix if this matters.
- Routes on one host with different prefixes are allowed. The proxy (pass B)
  matches a request by longest prefix. A prefix matches a request path when
  the path equals the prefix, or starts with it and the next character is `/`
  (or the prefix ends in `/`). So `/api` matches `/api` and `/api/users` but
  not `/apix`. `/` matches everything. Because identical host and prefix pairs
  are rejected, the longest match is always unique.

### Pass B2: route delivery and edge listeners (implemented)
- **Snapshot.** `RouteSnapshot` is `ServerMessage.routes` on the agent's Sync
  stream, next to assignments. `internal/server/routes.go` builds it
  (`routeSnapshot`) and `routePusher` sends it when the content changes, on
  each resync, and once with `edge: false` when the label is removed. A node
  without the label never gets one.
- **Contents.** All Routes, sorted by host, then longest prefix first. Each
  Route's endpoints are the Ready replica IPs in `Service.status.endpoints`
  (the same source the service proxy uses) at the target port of the
  Route's Service port (`resource.RouteEndpoints`). A Route whose Service
  or port is missing has no endpoints. `tls: secret` Routes carry the
  decrypted PEM certificate and key, sent only on the edge's stream.
- **Revision.** The server takes the revision from its clock in nanoseconds
  and makes it strictly increase per stream. `Edge.Apply` ignores any
  snapshot older than the one it applied.
- **Agent.** `internal/agent/edge.go` (`Edge`). `Apply` installs the table
  with `edge.Proxy.SetTable` and the certificates with `CertStore.Replace`.
  Listeners start on the first enabled snapshot and close when a snapshot
  says the node is not an edge. A rejected snapshot (for example two Routes
  on one host and prefix) leaves the previous state in place.
- **TLS in B2.** `tls: secret` uses the delivered certificate. `tls: auto`
  uses a self-signed certificate per host, kept in memory, and logs one
  warning per host until ACME exists (pass C). `tls: none` is HTTP only.
  Keys stay in memory and nothing is written to disk. When several Routes
  share a host, the first `secret` Route on that host supplies the
  certificate; `auto` is used only when no `secret` Route covers the host.
- **Secret format.** `tls: secret` reads PEM in the Secret's `tls.crt` and
  `tls.key` keys.
- **RouteStatus.** The controller writes it on every pass
  (`internal/controller/routes.go`). `state` is `Ready`, or the first reason
  the Route cannot serve yet: `ServiceNotFound`, `PortNotFound`,
  `NoEndpoints`, `SecretNotFound` or `SecretInvalid`. `message` says why.
  `cert_state` stays empty until pass C. `shoal get routes` shows STATUS and
  MESSAGE. This replaces the `Accepted`/`Rejected` values sketched in
  section 1. Those describe the apply-time checks, which pass A already does
  and which still run as before.
- **Labels.** `NodeSpec.labels`, set through the `SetNodeLabels` Control RPC
  and `shoal label node NAME KEY=VALUE` (`KEY-` removes a label).
- **Not done in B2:**
  - no on-disk cache of route snapshots: an edge restarted while the server
    is down serves no routes until it reconnects. Route config without keys
    could be cached later; this pass does not do it;
  - no real-host check. HTTP-01 and the ACME flow are pass C;
  - the reachability of container subnets from the host namespace, and a
    two-node run, are not verified;
  - ports 80 and 443 are not exercised anywhere. Tests use 127.0.0.1:0
    only, and the VPS is not run as an edge.

### Pass C1: ACME issuance and renewal (implemented)
- **Kinds.** `Certificate` (name = host; spec holds the chain and key PEM,
  status holds notBefore, notAfter, issuer, lastError, nextAttempt and the
  failure count), `AcmeAccount` (name = directory URL; spec holds the account
  key, directory and email; status holds the registration URI) and
  `AcmeChallenge` (name = token; spec holds host, key authorization and
  expiry). All three are internal and not user-appliable. They are in
  `encryptedKinds` (`internal/server/server.go`) with `Secret`, so specs are
  sealed on disk and in backups. Restore checks the same list.
- **Flags.** `shoal server --acme-directory` (default Let's Encrypt
  production, `https://acme-v02.api.letsencrypt.org/directory`; staging is
  `https://acme-staging-v02.api.letsencrypt.org/directory` and is the choice
  for any test that reaches a CA), `--acme-email`, `--acme-disable` (routes
  stay `CertPending`), `--acme-ca-roots` (PEM roots for a test CA such as
  pebble). An empty directory in the library disables the manager.
- **Client.** `acmeClient` in `internal/server/acme.go` covers register,
  authorize order, HTTP-01 lookup, accept, wait order, create cert and fetch
  cert. The real implementation wraps `golang.org/x/crypto/acme`. Registration
  passes `acme.AcceptTOS`, so the first certificate accepts the CA's terms
  for the configured account. Rate-limit answers become `rateLimitError`
  with the Retry-After delay.
- **Manager.** `certManager` (`internal/server/certs.go`) runs only inside
  `runLeaderTasks`, so it is leader-gated like the controllers. For each
  `tls: auto` host it issues when the Certificate is missing, renews when
  less than 30 days remain before notAfter, and honours `nextAttempt`. Failure
  backs off 1m, 2m, 4m, and so on, capped at 24h, and a Retry-After longer
  than the backoff wins. The error goes to `lastError`. Certificates of hosts
  with no `auto` Route are deleted.
- **Order.** Write one `AcmeChallenge` per token, wait for the challenge
  signal (below), call Accept, wait for the order, create the CSR and
  certificate, store the result, then delete the challenge records. Deletion
  runs on success and on failure.
- **Challenge signal.** The server records, per connected agent stream, whether
  the node is an edge and which tokens its last snapshot carried. The manager
  waits until at least one edge is connected and every connected edge has been
  sent every token, with a 2-minute cap. The signal is "sent": the snapshot has
  been handed to the node's gRPC stream. It does not prove the edge applied it.
  The gap is one stream send against a CA round trip after Accept, so it is
  narrow but not zero. An acknowledgement carried on the heartbeat would close
  it. That is left for a later pass.
- **Snapshot.** `RouteSnapshot` gained `challenges` (unexpired tokens) and
  `certificates` (the chain and key for each `auto` host with an issued
  certificate; edges only). The edge serves challenges from a
  `ChallengeStore` on the HTTP listener only. Unknown tokens get 404, and the
  HTTPS listener returns 404 for the challenge path. An issued certificate
  replaces the self-signed fallback by SNI through `CertStore.Replace`, with
  no restart. Precedence is Secret, then issued, then self-signed.
- **Route status.** The controller is still the only writer of Route status.
  For `tls: auto` it reads the host's Certificate: `CertPending` with no chain,
  `CertIssued` with `cert_not_after`, or `CertError` with `cert_message` when
  the last attempt failed. `shoal get routes` adds CERT and CERT EXPIRES
  columns.
- **Deviations from Pass C in the plan.** The manager checks every minute, not
  every 12 hours, so a new Route issues promptly. The 30-day renewal window is
  unchanged. The ADR's "rate limit tracked in status" is implemented as
  `nextAttempt` plus the Retry-After wait; no separate directory-limit table
  exists.
- **Not verified.** No real ACME server was used, so Let's Encrypt and pebble
  are untested. The tests use an in-process fake CA. The pebble CI pass (D)
  is the first real protocol test. The CERT columns of `shoal get routes`
  were not checked against live output.

## Open questions
- Resolved: see Decisions 1 to 4 above.
- Exact Let's Encrypt limits and their current values (verify in pass C).
- How the edge reports certificate expiry to operators: a status field on
  Route, a portal view, or both.
- Exact Let's Encrypt limits and their current values (verify in pass C).
- How the edge reports certificate expiry to operators: a status field on
  Route, a portal view, or both.
