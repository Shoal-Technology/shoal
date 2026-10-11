# shoal

A lightweight container orchestrator for small clusters (1–20 nodes): one
static binary that runs as control-plane **server**, worker **agent** or
**CLI**, running OCI containers through containerd and continuously
reconciling actual state toward declared desired state.

**Status:** Phase 5 of 6, plus a web portal. Done so far:
- Workloads are scheduled, health-checked, rolled out gradually and rolled
  back automatically.
- Secrets (encrypted at rest) and Configs are used as env vars or files.
- Services get a stable IP and the DNS name `<name>.shoal` on every node.
- Nodes are connected by a WireGuard mesh.
- Everything can be done with clicks in the built-in web portal.

Phase 6 (high availability) status: the control plane runs as a Raft cluster
of three servers, with join, write redirection, membership, backup and
restore. The `HA chaos` gate (`.github/workflows/ha-chaos.yml`) verified
leader kill, rejoin, a frozen leader, backup/restore and node removal on one
hosted runner, using loopback addresses only. Multi-host HA and agent
failover are not verified yet. See [docs/testing.md](docs/testing.md),
[docs/architecture.md](docs/architecture.md) and the
[decision records](docs/decisions/).

## Requirements

- Linux (amd64 or arm64), run as root
- containerd 2.x listening on `/run/containerd/containerd.sock`
- CNI plugins `bridge`, `host-local`, `portmap`, `firewall` and `loopback` in
  `/opt/cni/bin` (from [containernetworking/plugins](https://github.com/containernetworking/plugins/releases))
- `iptables` and `ip` (iproute2); for several nodes, the WireGuard kernel
  module and UDP port 51820 open between nodes
- Go (version in `go.mod`) to build

## Build

```sh
make build        # -> bin/shoal (static, CGO_ENABLED=0)
```

## Try it: single node

The quickest way to start is a server and an agent in one process:

```sh
sudo ./bin/shoal server --with-agent     # control plane + this machine as a node
sudo ./bin/shoal apply -f app.yaml       # app.yaml: see below
sudo ./bin/shoal get workloads           # READY 3/3 once the image is pulled
```

## Deploy an app

An app is one `shoal.yaml` (ADR 0017). [examples/shoal.yaml](examples/shoal.yaml)
is a web app with a Postgres database, a volume, a health check and a domain.
Shoal expands it into the Workload, Service, Route, Database, Volume and Secret
objects, all labelled `app=blog`. Before deploying, point the domain's DNS record
at an edge node (see [Ingress and automatic TLS](#ingress-and-automatic-tls)).

```sh
./bin/shoal deploy --dry-run -f examples/shoal.yaml   # create, update or unchanged per object
printf '%s' "$SESSION_KEY" | ./bin/shoal app secret set blog SESSION_KEY=-   # value from stdin
./bin/shoal deploy -f examples/shoal.yaml             # apply, wait for ready, print the URL
./bin/shoal app status blog                           # every object of the app, with its state
./bin/shoal app logs blog -f --tail 50                # replica logs, prefixed by replica name
./bin/shoal deploy -f examples/shoal.yaml --prune     # delete objects you dropped from the file
./bin/shoal app delete blog                           # keeps the Volume, Database and Secret
./bin/shoal app delete blog --data --yes              # also deletes them (asks without --yes)
```

- `deploy` applies everything as one batch, then waits (default `--wait=5m`)
  until the workload has its replicas ready, the database is Ready and the route
  is Ready. On timeout it names the object that is not ready and its status
  message, and exits non-zero. Re-running is safe.
- `--prune` never deletes a Volume, Database or Secret unless `--prune-data`
  is also given.
- A Secret is created empty by deploy and is never overwritten by it. Values
  are set only with `app secret set`, which never prints them.
- Deploy refuses to take over an object that has the same name but no
  `app=<name>` label.

## Web portal

Open `https://<server>:7444/` and log in as **admin**. The first password is
in `/var/lib/shoal/server/ui-initial-password` on the server; set a new one
with `sudo shoal ui reset-password`.

The browser warns about the certificate until you trust the cluster CA,
downloadable from the login page. Alternatively use your own certificate
with `--ui-cert/--ui-key`.

Everything is clickable: workloads (create, scale, restart, edit, delete,
live logs), nodes (Add node gives you the join command), Services, Secrets
and Configs. To keep the portal off the network, use
`--ui-listen 127.0.0.1:7444` and an SSH tunnel; `--ui-listen ""` disables it.

## State store

By default the server keeps its state in SQLite (`--store=sqlite`). With
`--store=raft` the state is kept in a Raft log under `<data-dir>/raft`
instead (ADR 0014). Several servers can share one raft cluster; see
[Cluster membership](#cluster-membership) below.

```sh
sudo ./bin/shoal server --store=raft
```

- `--store`: `sqlite` (default) or `raft`.
- `--raft-bind`: address Raft listens on, default `127.0.0.1:7445`.
- `--raft-node-id`: this node's name in the Raft cluster, default the host
  name. Keep it the same across restarts.

Switching an existing data dir between backends does not migrate data. The
server starts with an empty store on the other backend.

## Cluster membership

Servers with `--store=raft` can form one cluster. Each new server first needs
the cluster CA and secret key. A join bundle carries both, so nothing has to
be copied by hand. On an existing server, as admin:

```sh
sudo ./bin/shoal cluster join-bundle --out /root/join.bundle
```

Move the file to the new server over a private channel, then start it:

```sh
sudo ./bin/shoal server --store=raft --raft-node-id=node-2 \
  --raft-join=10.0.0.5:7443 --raft-join-bundle=/root/join.bundle
```

`--raft-join-bundle` installs the CA and key into `--data-dir` before the
first start. It refuses if the data dir already holds a different CA or
secret key, and writes nothing in that case. Running again with the same
bundle changes nothing, so the flag can stay in the service unit.

Inspect and change the membership (admin):

```sh
sudo ./bin/shoal cluster status         # members, voters, leader, indexes
sudo ./bin/shoal cluster remove node-3  # remove a member by its node ID
```

- `status` lists each member with its raft address, voter and leader flags.
  It also shows the leader's API address and the commit and applied index of
  the server you asked.
- `remove` refuses to remove the last voter. It can remove the leader
  itself: the leader steps down once the change commits, and the others elect
  a new leader. Stop the removed server's process afterwards. A removed server
  that keeps running may try to start elections.

**Security.** A join bundle is a cluster secret. It holds the CA private key,
which can issue certificates for the cluster, and the key that decrypts
Secrets. `join-bundle` creates the file with mode 0600 and refuses to
overwrite one. Move it only over a channel you trust, and delete it once the
new server has started. Each bundle issue is logged on the server with a
warning and the caller's address.

## Backup and restore

`shoal backup --out <file>` writes every record in the cluster to a file in
the `shoal-backup/v1` format (JSON: revision, creation time, server version,
record count, and a SHA-256 of the records). The server takes one
point-in-time copy. On the raft store it is read on the leader after a Raft
barrier, so every acknowledged write is included; a backup sent to a follower
is forwarded to the leader. The file is created with mode 0600 and the
command never overwrites a file.

```sh
sudo ./bin/shoal backup --out /root/cluster.backup.json
```

Restore loads a backup into a new, empty data dir before the first start:

```sh
sudo mkdir -p /var/lib/shoal/restored
sudo install -m 600 secret.key /var/lib/shoal/restored/secret.key
sudo ./bin/shoal server --data-dir=/var/lib/shoal/restored --store=raft \
  --restore-from=/root/cluster.backup.json
```

- Restore creates a new cluster. It generates a new CA, so agents, admin
  credentials and the portal certificate are issued again. Other servers join
  the restored one with the usual join flow.
- The data dir must be empty. The only file allowed in it is the secret key.
- The file is verified first: format, record count and SHA-256. A damaged file
  is refused before anything is written.
- Secrets stay encrypted in the backup. Restoring them needs the same
  `secret.key` the backup was taken with. A backup with Secrets is refused if
  the key is missing or wrong, before anything is written.
- Restore is offline. Run it on a new data dir before the node's first start,
  never against a running server. It works the same with the default sqlite
  store (omit `--store=raft`).

**Security.** A backup holds every record, but Secrets stay sealed. Keep the
file together with the key that opens it, and treat it as sensitive: it still
carries the portal password hash and join state.

## Try it: cluster

On the server machine (add `--with-agent` to also run workloads on it):

```sh
sudo ./bin/shoal server        # data in /var/lib/shoal/server, listens on :7443
```

The first start creates the cluster CA and writes CLI credentials to
`/var/lib/shoal/server/admin.conf`. The CLI finds them there when run as root.
To use the CLI as a normal user, copy them to `~/.shoal/config` (mode 0600) or
set `SHOAL_CONFIG`. Then create a join token:

```sh
sudo ./bin/shoal token create --max-uses 1
```

It prints the command to run on each node, for example:

```sh
sudo ./bin/shoal agent --server 10.0.0.5:7443 --token abc123.0123456789abcdef --ca-hash sha256:...
```

The agent checks the server's CA against `--ca-hash` before sending the
token. Then it saves its credentials to `/var/lib/shoal/agent/credentials.conf`,
so later starts are just `sudo shoal agent`. The server picks the advertised
address itself (the first non-loopback IPv4). If agents can't reach that
address, set `--advertise-address host:port`.

```sh
sudo ./bin/shoal get nodes
# NAME   STATUS  ADDRESS    CPU  MEMORY  VERSION  LAST HEARTBEAT  AGE
# node1  Ready   10.0.0.6   2    7.5Gi   dev      3s ago          1m

sudo ./bin/shoal apply -f app.yaml        # workload/web created
sudo ./bin/shoal get workloads            # NAME  IMAGE  READY  GENERATION  AGE
sudo ./bin/shoal get replicas             # where each replica runs, status, restarts, IP
sudo ./bin/shoal describe workload web    # workload settings and its replicas
sudo ./bin/shoal get workload web -o yaml # stored object, defaults filled in
sudo ./bin/shoal delete replica web-x7k2p # it is replaced, possibly on another node
sudo ./bin/shoal delete workload web      # removes its replicas and containers
sudo ./bin/shoal delete node node1        # replicas move away; its agent must re-join
```

`app.yaml` uses the Workload format:

```yaml
kind: Workload
metadata: { name: web }
spec:
  image: nginx:1.27
  replicas: 3
  ports: [{ containerPort: 80 }]
  resources: { cpu: 200m, memory: 128Mi }
  healthCheck: { http: { path: /, port: 80 }, intervalSeconds: 5 }
  update: { maxUnavailable: 1 }
```

What to expect when things fail:
- **A container crashes:** its agent restarts it within seconds (back-off
  1s, doubling up to 5m). `get replicas` shows the restart count.
- **A node is lost:** it is `NotReady` 20s after its last heartbeat. 60s
  after the last heartbeat its replicas are replaced on other nodes.
- **The cut-off node comes back:** it removes the old containers.
- **The server is down:** agents keep their containers running and keep
  restarting crashed ones, even if an agent restarts meanwhile.
- **The server restarts:** this is safe. State is in `state.db`, and agents
  reconnect on their own.

**Updates.** Changing a workload's spec (e.g. its image) rolls it out
gradually. At most `update.maxUnavailable` replicas (default 1) are down at
a time, and each batch waits until the previous one passes its health
check. Watch it with `shoal get workloads` (`UP-TO-DATE`, `ROLLOUT`) or
`shoal describe workload web`.

The rollout fails if an updated replica isn't ready within
`update.progressDeadlineSeconds` (default 120) or restarts 3 times. The
workload is then **rolled back automatically** to the last version that
fully rolled out, and `ROLLOUT` shows `RolledBack` with the reason in
`describe`. Your spec is left as you applied it: fix it and apply again to
start a new rollout.

**Health checks** (`healthCheck: { http: { path, port } }` or
`{ tcp: { port } }`) run on the node every `intervalSeconds`, after an
optional `initialDelaySeconds`. A replica counts as ready after its first
passing check. It is restarted after `failureThreshold` failures in a row.

**Secrets, Configs and Services:**

```yaml
kind: Secret
metadata: { name: db }
spec: { data: { password: hunter2 } }        # encrypted at rest, never shown again
---
kind: Config
metadata: { name: site }
spec: { data: { index.html: "hello from shoal" } }
---
kind: Workload
metadata: { name: web }
spec:
  image: nginx:1.28-alpine
  replicas: 2
  ports: [{ containerPort: 80 }]
  env: [{ name: DB_PASSWORD, valueFrom: { secret: { name: db, key: password } } }]
  files: [{ path: /usr/share/nginx/html, config: site }]   # one file per key
  healthCheck: { http: { path: /, port: 80 } }
---
kind: Service
metadata: { name: web }
spec: { selector: web, ports: [{ port: 80 }] }
```

Any container reaches the Service as `http://web.shoal/` (or `http://web/`)
on any node, spread over the ready replicas. `shoal get services` shows its
IP and endpoints, and `shoal get secrets` shows keys, never values.

Changing a Config or Secret updates mounted files within seconds.
Environment variables keep their values until
`sudo shoal restart workload web`, which replaces the replicas with a rolling
update. The same command retries a rolled-back release.

**Volumes (node-local, ADR 0016):** a Volume is storage that survives container
restarts. It lives on one node.

```yaml
kind: Volume
metadata:
  name: pg-data
spec:
  size: 20Gi          # enforced by the loop backend
  retention: Retain   # or Delete: what happens to the data when the Volume is deleted
---
kind: Workload
metadata:
  name: db
spec:
  image: postgres:16
  volumes:
    - name: pg-data
      mountPath: /var/lib/postgresql/data
```

- A Volume is used by one Workload. A read-write volume needs `replicas` of 0 or 1.
- The first replica that mounts a Volume picks its node, and every later
  replica goes to that node. Data does not move between nodes.
- If that node is lost, its replica stays Pending and the Volume is `NodeLost`
  until the node returns. Nothing is replaced on another node, because the data
  is still on the old one. Restoring from a snapshot is not in this release.
- Volume sizes count against the node's disk when the scheduler places replicas.
- `--volume-backend` is `loop` by default and needs root and loop devices. If
  the node has none, the agent logs a warning and uses `dir`, which has no size
  limit. `shoal get volumes` shows the quota (`size` or `none`).
- Deleting a Volume with `retention: Retain` moves its data to
  `<state-dir>/volumes/.retained`. Remove it with `sudo shoal volume purge NAME`
  on that node. Standalone agents do not support volumes yet.
- Changing a bound Volume's size is refused: online resize is not supported.

```sh
sudo ./bin/shoal get volumes      # NAME, SIZE, NODE, STATE, WORKLOAD, RETENTION, QUOTA
```

**Managed databases (ADR 0016, pass C):** a Database is Postgres with its
storage, credentials and a stable endpoint. Shoal expands it into a Volume, a
Workload, a Service and a credentials Secret, and it owns them. Edit the
Database, not those objects: the server refuses changes to them.

```yaml
kind: Database
metadata:
  name: app-db
spec:
  engine: postgres
  version: "16.15"     # pinned image; minor upgrades in place
  size: 5Gi            # data storage, fixed after creation
  resources: { cpu: 500m, memory: 384Mi }
  backup:              # validated and stored; backups run in a later release
    schedule: 24h
    retain: 7
    targets: [volume]
```

- `version` takes a major (`"16"` runs the newest pinned 16.x, as the app spec
  emits) or an exact minor. The supported versions are 15.19, 16.14, 16.15 and
  17.11, each pinned by image digest.
- Credentials are generated once, from `crypto/rand`, and stored encrypted in
  the Secret `app-db-credentials`. Its `url` key is
  `postgres://app:<password>@app-db.shoal:5432/app_db?sslmode=disable`.
  Reference it with `valueFrom: { secret: { name: app-db-credentials, key: url } }`.
- A minor version change updates the image. A major version change is refused
  with a `Failed` status: restore a backup into a new Database of the new major.
  Backups before upgrades and restore are not in this release.
- Deleting the Database deletes its objects. The data Volume follows its
  retention (`Retain` by default, kept on the node).
- A database has one replica and no high availability.

```sh
sudo ./bin/shoal get databases    # NAME, ENGINE, VERSION, SIZE, PHASE, ENDPOINT, AGE, MESSAGE
```

**Logs:**

```sh
sudo ./bin/shoal logs web --tail 20      # all replicas, merged by time, prefixed with the replica name
sudo ./bin/shoal logs web -f             # follow
sudo ./bin/shoal logs web --replica web-x7k2p
```

## Try it: standalone agent

The agent reads desired replicas from [examples/standalone.yaml](examples/standalone.yaml)
and keeps them running. It re-reads the file when it changes.

```sh
sudo ./bin/shoal agent --standalone -f examples/standalone.yaml
```

In a second terminal:

```sh
sudo ./bin/shoal debug ps                 # containers, state, restarts, IP
curl http://<container-ip>/             # nginx (web-1) via its container IP
curl http://<node-ip>:8080/             # ...or via the published host port
sudo ./bin/shoal debug logs crashy -f     # follow logs; crashy restarts with growing back-off
```

Then edit `examples/standalone.yaml` and watch the agent log:

- add `desired: Stopped` to `web-1`: it is stopped and kept; remove the line and it starts again;
- change `image` or `env`: the container is recreated;
- delete a replica: its container is stopped and removed;
- break the YAML: the error is logged and the previous desired state is kept.

Stop the agent with Ctrl-C: **containers keep running**, and restarting the
agent picks them up again. To clean up, empty the list (`replicas: []`) and let
the agent remove everything, or use `sudo ./bin/shoal debug rm <id>...`.

Note: the agent owns its containerd namespace (`shoal`) and removes every
container there that is not in the file, including ones started with
`shoal debug run`. Use `--containerd-namespace` to experiment with `debug`
commands while an agent is running.

Useful flags: `--state-dir` (logs and runtime state, default `/var/lib/shoal`),
`--pod-cidr` (container subnet, default `10.250.0.0/24`), `--stop-timeout`,
`--sync-interval`, `--log-format json`.

## Ingress and automatic TLS

A `Route` maps a host and optional path prefix to a Service port. Nodes labelled
`shoal.dev/edge=true` run the edge: HTTP on `:80` and HTTPS on `:443` (set with
`--edge-http` and `--edge-https` on the agent; empty disables one).

```sh
sudo ./bin/shoal label node edge-1 shoal.dev/edge=true
sudo ./bin/shoal apply -f route.yaml     # kind: Route, tls: auto | secret | none
```

Automatic certificates come from an ACME CA (Let's Encrypt by default). The
server asks before it registers an account:

- `--acme-accept-tos` accepts the CA's terms of service. Without it no account
  is registered and every `tls: auto` Route reports
  `CertError: ACME terms not accepted; start the server with --acme-accept-tos`.
- `--acme-directory` selects the CA. Use the staging directory for tests:
  staging certificates are not trusted by browsers.
- `--acme-email` is the contact registered with the account (recommended).

Point DNS at the edge nodes yourself. Shoal does not manage A records, and it
serves HTTP-01 only, so the name must reach an edge on port 80.

Optional `--edge-cache-dir` on the agent keeps a copy of the routes and
endpoints (never certificates or keys), written 0600 and atomically. An edge
restarted while the server is down then keeps serving its HTTP routes.
Certificates still come only from the server, so `tls: secret` and `tls: auto`
hosts need a connection before they answer HTTPS. The cache is off by default.

## Tests

```sh
make test          # unit tests (any OS)
make vet           # go vet, including integration-tagged files
make integration   # against real containerd: needs Linux, root, CNI plugins and network access
make ingress-e2e   # ingress against pebble: needs CI=true or SHOAL_DISPOSABLE=1
make proto         # regenerate api/v1alpha1 after editing a .proto (needs protoc + plugins)
```

`make integration`, `integration-report` and `ingress-e2e` refuse to run unless
`CI=true` (GitHub Actions) or `SHOAL_DISPOSABLE=1` is set. They change host
networking and bind ports, so they must not run on a shared machine.

## License

Copyright 2026 the Shoal authors.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the
full text.
