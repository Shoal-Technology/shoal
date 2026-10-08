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

High availability comes in Phase 6. See
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

## Tests

```sh
make test          # unit tests (any OS)
make vet           # go vet, including integration-tagged files
make integration   # against real containerd: needs Linux, root, CNI plugins and network access
make proto         # regenerate api/v1alpha1 after editing a .proto (needs protoc + plugins)
```
