# 0017: Onboarding: init, the app spec and the 5-minute path

Status: Accepted

## Context
The goal of this ADR is one experience: a fresh 1 GB VPS to an app with a
database and an HTTPS domain in about five minutes, from one file and a few
commands.

Today the path is long. The README (Requirements, "Try it: cluster",
"Ingress and automatic TLS") asks the operator to:
- install containerd 2.x and the CNI plugins by hand, plus iptables and iproute2;
- run `shoal server`, copy `admin.conf` to `~/.shoal/config`, and create a join
  token with `shoal token create`;
- start `shoal agent --server ... --token ... --ca-hash ...` and label the node
  `shoal.dev/edge` (ADR 0015, pass B2) so that it serves ports 80 and 443;
- write Workload, Service, Route and, from ADR 0016, Volume and Database objects
  by hand and apply them with `shoal apply -f`.

Facts from the code that this ADR depends on:
- Kinds today: Workload, Node, Replica, Secret, Config, Service and Route
  (`api/v1alpha1/resources.proto`, decoded in `internal/resource/kinds.go`).
  Volume and Database are not in the proto yet; they land with ADR 0016.
- `ObjectMeta` has `name, uid, generation, revision, created_at, updated_at`
  and **no labels**. Labels exist only on `NodeSpec`. Ownership by label, which
  `shoal app delete` needs, therefore requires an additive proto change (see 2.4).
- `WorkloadSpec` has no volume field and no image pull secret field yet
  (ADR 0016 adds volumes).
- `ServiceSpec.selector` is a string. The exact selector format is confirmed in pass A.
- The agent takes `--token`, `--ca-hash` and `--server`, and saves credentials to
  `/var/lib/shoal/agent/credentials.conf`. It serves the edge on `--edge-http :80`
  and `--edge-https :443` while the node carries the `shoal.dev/edge` label.
- The server takes `--data-dir` (default `/var/lib/shoal/server`), writes
  `admin.conf` and `ui-initial-password` there, and takes `--acme-email`,
  `--acme-accept-tos`, `--acme-directory`, `--acme-ca-roots` and `--advertise-address`.
  It also takes `--with-agent`, which runs a server and an agent in one process.
- The CLI looks for credentials in this order: `--config`, `$SHOAL_CONFIG`,
  `~/.shoal/config`, then the server's `admin.conf`.
- `examples/route.yaml` still says that edge serving is not implemented. ADR 0015
  pass E has landed, so the comment is stale and should be fixed in pass B.

Two numbers constrain the design. A 1 GiB VPS shows about 950 MiB of usable
memory. A stock Postgres takes 2 GiB by default in ADR 0016 (section 3.1), which
does not fit on that host. The sections below set a smaller default for
`database: postgres`.

## Decision

### 1. `shoal init`, `shoal init --join`, `shoal uninstall`

**Status (pass E): done.** Built as described, with these differences, as
built:
- `--email` is optional. The ACME flags are passed to the server only when both
  `--email` and `--accept-tos` are given (the brief for pass E; the ADR text
  above asks for `--email` always).
- `--containerd-version` and `--upgrade-containerd` are not implemented. The pins
  are constants in `internal/install/versions.go`: containerd 2.4.1, runc 1.5.2,
  CNI plugins 1.9.1, with the SHA-256 of each release asset.
- CNI plugins are installed when one of the five plugins is missing. A plugin
  set that is present with another version is not replaced, because the plugins
  do not report a version.
- The agent gained `--token-file` (read once, then deleted). `shoal token create
  --bundle --out FILE` writes the `shoal-join/v1` bundle.
- `init --join` does not label the joined node. The label is applied from the
  cluster (the next steps say so).
- The containerd CRI disable key (`io.containerd.cri.v1.runtime` and
  `io.containerd.cri.v1.images`) is checked in CI: the containerd log shows the
  CRI runtime plugin is not registered.

#### 1.1 Synopsis
```
sudo shoal init --email ops@example.com [--accept-tos] [--node-name NAME]
                [--edge=true] [--swap 1G] [--open-firewall] [--dry-run]
sudo shoal init --join join.json [--edge]
sudo shoal uninstall [--dry-run] [--purge --yes]
```
Test-only flags, hidden from help: `--root-dir DIR` (every write is prefixed with
DIR), `--acme-directory`, `--acme-ca-roots` (pass to the server for pebble) and
`--containerd-version` (for the test matrix). Env `SHOAL_INIT_FAKE_HOST=FILE`
replaces the kernel, memory, disk and port probes with the values in FILE, so
that the dry-run golden test is stable on any CI runner.

Exit codes: `0` done or nothing to do; `1` a step failed (the message names the
step); `2` a preflight check failed and nothing was changed; `3` partly applied,
so re-run to continue.

#### 1.2 Steps (in order)
Each step is idempotent. It first reads the desired state, prints `ok` if
nothing changes, and writes only the difference. `--dry-run` prints every step
as `WOULD <action> <path or URL> <detail>` and performs no write and no
download.

1. **Preflight (read-only).** Checks, with the failure level in brackets:
   - root, Linux, arch `amd64` or `arm64` [fail];
   - systemd running (`/run/systemd/system` exists) [fail];
   - cgroup v2 (`/sys/fs/cgroup/cgroup.controllers`) [fail];
   - overlayfs in `/proc/filesystems`, and `br_netfilter` loadable (`modprobe -n`) [fail];
   - `iptables` and `ip` on PATH [fail];
   - kernel 5.10 or newer [warn];
   - `/dev/loop-control` present; without it, volumes use `--volume-backend=dir`
     as ADR 0016 section 1.2 allows [warn];
   - ports 80, 443, 7443 and 7444 free, or held by a shoal unit [fail]; 51820 only for `--join` or multi-node [fail];
   - at least 8 GiB free on the filesystem that holds `/var/lib` [fail], and 15 GiB or more [warn];
   - MemTotal of at least 900 MiB [fail]; under 2 GiB with no swap [warn, and the hint offers `--swap 1G`];
   - a containerd, if present, must be 2.x [fail] (see 1.3).
   Any failure exits `2` before any write.
2. **Kernel settings.** Write `/etc/sysctl.d/99-shoal.conf` with
   `net.ipv4.ip_forward=1` and `net.bridge.bridge-nf-call-iptables=1`, load
   `br_netfilter`, and apply the settings. The CNI bridge (ADR 0012 Service
   iptables) needs both.
3. **containerd.** Reuse an existing containerd 2.x listening on
   `/run/containerd/containerd.sock` without touching its config. Otherwise
   install the pinned static release (1.3). Shoal never installs Docker and never
   replaces a containerd it did not install.
4. **CNI plugins.** Install the pinned `containernetworking/plugins` release
   into `/opt/cni/bin` if any of `bridge`, `host-local`, `portmap`, `firewall`
   or `loopback` is missing or has the wrong version.
5. **containerd config.** Only when init installed containerd: write
   `/etc/containerd/config.toml` with the CRI plugin disabled, because Shoal
   talks to containerd through its client API and not through CRI. The exact
   disabled-plugin key for containerd 2.x is pinned and tested in pass E.
6. **systemd units.** Write these files and run `systemctl daemon-reload`, then
   `systemctl enable --now` only for units whose content changed:
   - `shoal-server.service`: `ExecStart=/usr/local/bin/shoal server --listen :7443
     --ui-listen :7444 --data-dir /var/lib/shoal/server --acme-email <email>`,
     plus `--acme-accept-tos` when the operator passed `--accept-tos`
     (see 1.5). `Restart=always`.
   - `shoal-agent.service`: `ExecStart=/usr/local/bin/shoal agent --server
     127.0.0.1:7443 --token-file /run/shoal/join.token --ca-hash sha256:<hash>
     --node-name <name> --edge-http :80 --edge-https :443`. `After=containerd.service
     shoal-server.service`, `Restart=on-failure`. `--token-file` is a new agent
     flag (pass E); the token file is read once and then deleted.
   - `containerd.service` is the upstream unit, pinned as a copy, and written
     only when init installed containerd.
   The server and the agent run as **two units**, not as `server --with-agent`.
   That keeps `journalctl -u` clear, and the same agent unit serves a joined node.
7. **Server bootstrap.** On the first start the server creates the cluster CA,
   the secret key and `admin.conf`. Init waits up to 60 s for `admin.conf` and
   for a successful status call, and then reads the CA hash from the data dir.
8. **Local agent and edge label.** Init creates a single-use token
   (`token create --max-uses 1 --ttl 10m --description init-local`), writes it
   to `/run/shoal/join.token` (mode 0600), and waits up to 60 s for the node to
   register. It then runs `shoal label node <name> shoal.dev/edge=true` (skipped
   with `--edge=false`).
9. **Admin config.** Copy `admin.conf` to `~/.shoal/config` of `$SUDO_USER`
   (or of root when run directly), mode 0600, owned by that user. Other home
   directories are never touched.
10. **Next steps.** Print the commands for the next steps: the UI password path
    (`/var/lib/shoal/server/ui-initial-password`) and how to reset it
    (`sudo shoal ui reset-password`), the DNS record to create, `shoal deploy`,
    and the `--join` command for another node. Init never prints a password.

#### 1.3 Containerd: choice, pins and fallback
Decision: **the official static release** of containerd 2.x (the
`containerd-<version>-linux-<arch>.tar.gz` asset of the containerd GitHub
release), with its SHA-256 checked against the release's published checksum
file. The version and both hashes are constants in source (`internal/install/versions.go`),
per architecture. The CNI plugins are pinned the same way.

Options considered:

| Option | For | Against |
|---|---|---|
| A. Official static release (chosen) | One code path on every distro; the same bytes on every host, so dry-run output and golden tests are meaningful; no package manager dependency | Shoal owns security updates; mitigated by a pin bump in CI and `shoal init --upgrade-containerd` |
| B. Distro packages (apt, dnf, apk) | Distro owns security updates | Versions lag (Debian 12 ships containerd 1.6, below the 2.x that README requires); package names, paths and unit files differ, so init needs one code path per distro |
| C. Reuse only, never install | Simplest | Pushes the hardest step onto the operator, the thing this ADR removes |

Rule: if containerd is already present and is 2.x, reuse it (A and C agree).
Otherwise install A. `--containerd=distro` is the escape hatch: it uses apt or
dnf when present, checks that the result is 2.x, and fails with a hint
otherwise. The version pin is recorded in `/var/lib/shoal/init.json`, so
uninstall knows what to remove.

Open: whether to verify the release with cosign or GitHub attestations in
addition to SHA-256 (see Open questions).

#### 1.4 What init never does
- Never changes the firewall or cloud security groups. The only exception is
  `--open-firewall`, which adds `ufw allow 80/tcp`, `443/tcp` (and `51820/udp`
  for joins) when ufw is installed and active. It never enables ufw and never
  removes rules.
- Never creates or changes swap unless `--swap SIZE` is given.
- Never installs Docker or any other container runtime.
- Never reconfigures, restarts or removes a containerd it did not install.
- Never requests a certificate. Certificates come at deploy time, when a Route
  exists.
- Never prints or logs the admin or UI password, and never writes it to the
  journal.
- Never edits SSH configuration, users, unattended-upgrades or `/etc/hosts`.
- Never deletes anything under `/var/lib/shoal` except through `uninstall --purge --yes`.
- Never runs against a host without the disposable marker in test jobs (section 6).

#### 1.5 ACME and email
Certificates need a contact email and an accepted terms of service (ADR 0015,
pass C1). Init therefore requires `--email` (`--acme-email`). Init does not
accept the Let's Encrypt terms on the operator's behalf. Without
`--accept-tos`, the server starts with ACME disabled, and the first deploy with
a Route reports `CertError: ACME terms of service not accepted` (error E06 in
section 4), which tells the operator to re-run init with `--accept-tos`. The
flag is an explicit consent step.

#### 1.6 Join
On the first node: `shoal token create --bundle --out join.json --max-uses 1
--ttl 30m`. The new flag writes a `shoal-join/v1` file (mode 0600) containing
the server address, the token, the CA hash and the TTL. On the new host,
`sudo shoal init --join join.json [--edge]` runs preflight, installs containerd
and the CNI plugins, writes only the agent unit, and deletes the bundle after
use (`--keep-bundle` keeps it). The agent credentials are saved as before.

Joining does not change DNS. Several edges need several A records, and that is
an operator step (open question 4).

#### 1.7 Uninstall
`shoal uninstall` stops and disables both units, removes their unit files and
the sysctl file, and removes containerd and the CNI plugins **only** if the
install record in `/var/lib/shoal/init.json` says init installed them. If
containers outside namespace `shoal` are still running, it lists them and
leaves containerd in place. Data under `/var/lib/shoal` (CA, secret key,
state, volumes) is kept by default. `--purge --yes` deletes it, and the
command prints a warning that Secrets and volume data go with it. It never
deletes `~/.shoal`, which is the operator's own file, and it prints the path.

### 2. The app spec: `shoal.yaml`

#### 2.1 Example (one app, about 20 lines)
```yaml
name: blog                         # required, DNS label, max 40 chars
image: ghcr.io/example/blog:1.4.2  # required; a tag or digest (warn on :latest)
port: 3000                         # container port; required with domain
domain: blog.example.com           # optional; one host, path "/"
replicas: 1                        # default 1; >1 requires no volumes
env:
  LOG_LEVEL: info
  DATABASE_URL: ${db.url}          # ${db.url|host|port|name|user} from database:
secrets:                           # keys in Secret blog-secrets; values via `shoal app secret set`
  - SESSION_KEY
database: postgres                 # or {engine: postgres, version: "16", size: 5Gi}
volumes:
  - name: uploads
    mountPath: /app/uploads
    size: 5Gi
health:
  path: /healthz                   # HTTP readiness on the container port
resources: { cpu: 250m, memory: 256Mi }
```
`build:` is accepted by the schema as a reserved key. In v1 it fails with
`build is not supported; push an image (see docs)`, which keeps Shoal out of
image building (open question 2). Unknown keys fail with "did you mean".

#### 2.2 Expansion
`name` is `blog` in the table below. The shoal.yaml above expands to these objects.

| Object | Name | Source | Notes |
|---|---|---|---|
| Workload | `blog` | image, env, secrets, volumes, health, resources, replicas, port | `ports` from `port`; health from `health.path` |
| Service | `blog` | `port` with `domain` | service port 80, target port `port` |
| Route | `blog` | `domain` | host `blog.example.com`, pathPrefix `/`, service `blog`, port 80, tls `auto` |
| Database | `blog-db` | `database` | engine `postgres`, version `16`, size `5Gi`, memory request 384Mi (see below) |
| Volume | `blog-uploads` | `volumes[]` | ADR 0016 Volume kind, retention `Retain` |
| Secret | `blog-secrets` | `secrets[]` | created empty if missing; deploy never overwrites data |

Expanded YAML, exact:
```yaml
apiVersion: v1alpha1
kind: Workload
metadata:
  name: blog
  labels: {app: blog, shoal.dev/managed-by: app}
spec:
  image: ghcr.io/example/blog:1.4.2
  replicas: 1
  ports: [{containerPort: 3000}]
  env:
    - {name: LOG_LEVEL, value: info}
    - name: DATABASE_URL
      valueFrom: {secret: {name: blog-db, key: url}}
    - name: SESSION_KEY
      valueFrom: {secret: {name: blog-secrets, key: SESSION_KEY}}
  volumes: [{name: blog-uploads, mountPath: /app/uploads}]
  healthCheck: {http: {path: /healthz, port: 3000}, intervalSeconds: 10, timeoutSeconds: 2, failureThreshold: 3}
  resources: {cpu: 250m, memory: 256Mi}
---
kind: Service
metadata: {name: blog, labels: {app: blog, shoal.dev/managed-by: app}}
spec: {selector: blog, ports: [{port: 80, targetPort: 3000, protocol: tcp}]}
---
kind: Route
metadata: {name: blog, labels: {app: blog, shoal.dev/managed-by: app}}
spec: {host: blog.example.com, pathPrefix: /, service: blog, port: 80, tls: auto}
---
kind: Database
metadata: {name: blog-db, labels: {app: blog, shoal.dev/managed-by: app}}
spec: {engine: postgres, version: "16", size: 5Gi, resources: {cpu: 500m, memory: 384Mi}}
---
kind: Volume
metadata: {name: blog-uploads, labels: {app: blog, shoal.dev/managed-by: app}}
spec: {size: 5Gi, accessMode: RWO, retention: Retain}
---
kind: Secret
metadata: {name: blog-secrets, labels: {app: blog, shoal.dev/managed-by: app}}
spec: {data: {}}
```
Notes on the expansion:
- The `DATABASE_URL` value is never written in plain text. It is a reference to
  the credential Secret that the Database controller creates (ADR 0016 section
  3.2). The exact Secret name and key (`blog-db`, `url`) are assumptions to confirm
  when ADR 0016 pass C lands.
- `database: postgres` defaults to `memory: 384Mi` and `cpu: 500m`, and not to
  ADR 0016's 2 GiB default, so that it fits on a 1 GiB host with the server and
  agent. The Postgres configuration (`shared_buffers` in the 64 MiB range,
  low `max_connections`) is a Database controller default that pass C must set.
- `replicas > 1` with any volume is a validation error.
- Expansion is a pure function: the same input gives the same objects, in the
  order above.

#### 2.3 Naming and ownership
- Fixed names: Workload, Service and Route `<name>`; Database `<name>-db`;
  Volume `<name>-<volume>`; Secret `<name>-secrets`; the generated credential
  Secret `<name>-db`.
- Every expanded object carries labels `app=<name>` and
  `shoal.dev/managed-by=app`.
- Deploy refuses to overwrite an object with the same name that lacks the
  label `app=<name>`. The error is E13 (section 4).
- A Route host can belong to one app only. A second app with the same host is
  refused (E13).

#### 2.4 Required proto change
`ObjectMeta` gets `map<string, string> labels = 7;` (additive, `api/v1alpha1`).
Older servers drop the field. Deploy checks the server version first and refuses
to deploy labelled objects to a server without label support, with a hint to
upgrade the server.

#### 2.5 Commands
- `shoal deploy shoal.yaml [--dry-run] [--yes] [--timeout 10m]`. It validates,
  expands, fetches the objects with `app=<name>`, prints a plan (create, update,
  unchanged, delete, with a per-field diff), and asks for confirmation on a TTY
  (`--yes` skips it). It then applies in this order: Volume, Database,
  Secret (create only), Workload, Service, Route, and finally deletes of
  orphans. It waits until the Database is `Ready`, the Workload has
  `readyReplicas == replicas` and the Route has a certificate or reports
  `CertPending` for at most 120 s. Then it prints the summary:
  ```
  blog  https://blog.example.com  ready in 3m12s
    database  blog-db       Ready     postgres 16   blog-db.shoal:5432
    workload  blog         1/1 ready image ghcr.io/example/blog:1.4.2
    volume    blog-uploads  5Gi       bound to edge-1
    route     blog          TLS issued, expires 2027-01-08, DNS ok (203.0.113.10)
  ```
  Exit codes: `0` ready; `1` failed (prints the E-code and hint); `3` still
  progressing at the timeout (re-run is safe).
- `shoal app status blog [-o json]`. The same summary from live state, plus the
  last five events and any open E-codes.
- `shoal app logs blog [--db] [--replica ID] [--follow] [--tail N] [-t]`.
  Tails the workload replicas by default, or the database with `--db`. It wraps
  the existing `shoal logs WORKLOAD` path.
- `shoal app delete blog [--delete-volumes] [--yes]`. Deletes every object with
  `app=blog`, and prints the list before it deletes. Volumes are kept by default
  (retention `Retain`, ADR 0016 section 1.6) and stay labelled, so that
  `shoal app status` can show them as kept. `--delete-volumes` removes them as
  well. Database deletion follows its own retention rules.
- `shoal app secret set blog SESSION_KEY` reads the value from stdin or a
  prompt. It writes only the named key of `blog-secrets`.

#### 2.6 Library and dry-run diff
Decision: **the expansion is a library, `internal/appspec`, used by the CLI
only, for now.** It has `Parse`, `Validate`, `Expand(spec) ([]*v1alpha1.Object, []Warning, error)`
and `Diff(expanded, live)`.

Reasons:
- The server API does not change. Older and newer CLIs and servers keep the
  same kinds on the wire.
- `shoal deploy --dry-run` diffs against live state with one read and no
  server-side logic, and the library is testable offline with golden files.
- The server never needs to know `shoal.yaml`, so the expansion is not duplicated.
- A server-side `App` kind would need a migration and a second copy of the rules,
  and it adds nothing the CLI cannot already do.

The library lives in the same Go module, so the web portal or the server can
import it later without a copy. That is a later pass, not part of v1.

### 3. `shoal import compose docker-compose.yml`

```
shoal import compose docker-compose.yml [--service NAME] [--out shoal.yaml] [--strict]
```
- With one non-database service that publishes a port, that service is chosen.
  Otherwise `--service` is required, and the error lists the candidates.
- The output is a valid `shoal.yaml` (checked with `appspec.Validate`). A header
  comment lists every dropped key, so the file records what was lost.
- Warnings go to stderr, one line each, grouped by service. `--strict` exits `1`
  when anything is unsupported.

Mapping:

| Compose key | shoal.yaml | Result |
|---|---|---|
| `image` | `image` | kept (tag or digest; `:latest` warns) |
| `build` | none | error with the hint "push an image" (v1) |
| `ports` | `port` | the first container port becomes `port`; host ports are dropped with a warning (Shoal routes by domain); more ports warn |
| `environment`, `env_file` | `env` | literal values kept. Keys that look like secrets (PASSWORD, SECRET, TOKEN, KEY, PASS) move to `secrets:` and a comment says to run `shoal app secret set`. `env_file` is read if present, else warned |
| `${VAR}` interpolation | resolved | from `.env` or the process env; otherwise kept and warned |
| `volumes` (named) | `volumes` | `size` defaults to `5Gi` and warns that it was guessed |
| `volumes` (bind) | none | unsupported, printed as "host paths are not portable; use a named volume" |
| `image: postgres:*` | `database: {engine: postgres, version: "<major>"}` | the service is replaced by a Database; its volume becomes `size` |
| `image: redis:*` | `database: redis` | Database (engine redis, ADR 0016) |
| `POSTGRES_*`, `REDIS_*` on a DB service | dropped | warned: Shoal generates the credentials and the app gets `${db.url}` |
| `depends_on` | none | ignored with a warning: Shoal has no start order yet, so the app must retry the database |
| `command` | `command` | kept |
| `entrypoint` | none | unsupported: Workload has no entrypoint field (warn) |
| `healthcheck` (`curl`/`wget` on HTTP) | `health.path` | mapped when the path is obvious; otherwise warned |
| `restart` | none | ignored (Shoal's restart policy is fixed) |
| `deploy.replicas` | `replicas` | kept; >1 with volumes errors |
| `deploy.resources.limits` | `resources` | kept |
| `labels` (incl. Traefik labels) | none | unsupported; the warning says to use `domain:` |
| `networks`, `network_mode`, `privileged`, `cap_add`, `user`, `working_dir`, `logging`, `profiles`, `extends`, `configs`, top-level `secrets` | none | unsupported, printed per key |

Golden tests cover each row (section 6).

### 4. Errors that explain fixes

Each error has a stable code, a human message with the concrete values, and a
fix hint. The code and the hint live in `internal/errs` (pass D). Surfaces:
**CLI** (`shoal deploy` failure output and exit `1`), **status** (`shoal app
status` and `-o json`), and **portal** (the banner on the app page, pass D in the
web portal, ADR 0013).

| Code | Condition | Message (example) | Fix hint | Surfaces |
|---|---|---|---|---|
| E01 | Image pull unauthorized | `pull ghcr.io/example/blog:1.4.2: unauthorized` | Add a pull secret; `image_pull_secret:` needs the new WorkloadSpec field (open question 3) | CLI, status, portal |
| E02 | Image or tag not found | `pull ...: manifest unknown` | Check the tag; `:latest` is not pinned | CLI, status |
| E03 | Edge port in use | `edge port 443 is held by nginx (pid 812); Shoal cannot serve HTTPS` | Stop the process, or move it. `shoal init` runs this check | init, CLI, status |
| E04 | No edge node | `no node carries shoal.dev/edge; route blog.example.com will not be served` | `shoal label node NAME shoal.dev/edge=true`, or re-run init on the host | CLI, status, portal |
| E05 | DNS does not point at an edge | `blog.example.com resolves to 198.51.100.7; edges are 203.0.113.10` | Set the A record to an edge IP, and wait for TTL | CLI, status, portal |
| E06 | ACME terms not accepted | `CertError: ACME terms of service not accepted` | Re-run init with `--accept-tos` (section 1.5) | CLI, status, portal |
| E07 | HTTP-01 challenge fails, or rate limited | `certificate: HTTP-01 challenge for blog.example.com failed: connection refused on port 80`; or `rate limited until 2026-10-12` | Open port 80 in the cloud firewall; after a rate limit, wait or use the staging directory | CLI, status, portal |
| E08 | Insufficient disk | `need 5Gi for volume blog-uploads; 2.1Gi free under /var/lib/shoal` | Free space, or reduce `size` | CLI (preflight), init, status |
| E09 | Insufficient memory (Pending) | `replica blog-1 is Pending: 512Mi requested, edge-1 has 300Mi free` | Lower `resources.memory`, or add a node | CLI, status, portal |
| E10 | Volume's node is lost | `volume blog-uploads is on node edge-1, which is NotReady since 14:02` | Bring the node back, or restore a snapshot (ADR 0016 section 1.5) | status, portal |
| E11 | Database not ready or failed | `blog-db Failed: container exited 1: initdb: could not change permissions of directory` | Check the volume owner and mode (ADR 0016 section 1.1); `shoal app logs blog --db` | CLI, status, portal |
| E12 | Missing Secret value | `secret blog-secrets key SESSION_KEY has no value` | `shoal app secret set blog SESSION_KEY` | CLI, status, portal |
| E13 | Name or host owned by another object | `route blog.example.com is already used by app blog-old` | Pick another domain, or `shoal app delete blog-old` | CLI, status |
| E14 | Crash loop | `replica blog-1 crashed 4 times; last exit 1: "panic: missing DATABASE_URL"` | `shoal app logs blog`; fix the app or its env | CLI, status, portal |
| E15 | Health check failing | `replica blog-1 not ready: GET /healthz on :3000 returned 404` | Set `health.path` to a route the app serves | CLI, status, portal |

Each message prints the object name, the node and the time. The portal shows
the same text in its banner, and `status -o json` returns the code, so that
scripts can match on it.

### 5. Low-overhead targets

Targets, measured on a 1 GiB host, a single node with the server and the
agent, and the sqlite store:

| Metric | Target | Source |
|---|---|---|
| Idle RSS of `shoal-server` | at most 80 MiB | `VmRSS` in `/proc/<pid>/status`, sampled 120 s after start with no workloads |
| Idle RSS of `shoal-agent`, edge on | at most 40 MiB | same |
| Idle RSS of containerd | at most 80 MiB (measured, not owned) | same |
| Init to first HTTPS 200 for the `blog` example | p50 at most 5 min, p95 at most 8 min | from `shoal init` start to the first 200 from `https://blog.example.com` with a valid certificate |

**Measured (pass E, run 38164124780, commit 2a0ad52, Ubuntu 24.04.5 x86_64
hosted runner, not the 1 GiB VM):** init 6294 ms; idle RSS of `shoal-server`
42892 kB, of `shoal-agent` (edge on) 42848 kB, of containerd 45468 kB; init start
to the edge routing the app's host 8358 ms (the edge answers HTTP 308 to HTTPS,
and the app answers 200 on its replica). Without ACME terms no certificate is
issued, so the HTTPS first-response time is not measured yet. This is one
recorded run; the regression gate needs three.

The init-to-HTTPS target includes image pulls (Postgres is about 150 MB
compressed) and the certificate. It is reported for the first three runs and
gated after that, at a regression of more than 25% over the rolling median.

Benchmark script `scripts/bench/onboard.sh`:
- takes the SSH target as an argument. It refuses to run unless the host has
  `/etc/shoal-disposable` (the marker written by the CI job), so the shared
  VPS is refused by design;
- runs `shoal init`, deploys the blog example from section 2 with a real domain
  or a pebble domain, polls HTTPS, and samples RSS after 120 s idle;
- writes `bench/results/onboard-<date>-<arch>.json` with the timings, RSS and
  host info (CPU, memory, kernel, containerd version).

Approximation: the hosted CI runner has 7 GB, so the job caps the test with
`systemd-run --scope -p MemoryMax=1G -p MemorySwapMax=0`. That approximates a
1 GiB host. A real 1 GiB VM run is the nightly job on a disposable VM.

CI job `onboard-bench`: nightly and on release tags. It creates a disposable
VM (1 GiB, arm64 and amd64 in two matrix rows), runs the script, uploads the
JSON as an artifact, and opens a PR that appends a row to
`docs/benchmarks/onboarding.csv`. The VM is destroyed at the end of the job.

### 6. Testing

- **Golden tests, expansion.** `internal/appspec/testdata/*.yaml` with
  `*.expanded.yaml` next to them, and `go test ./internal/appspec -update` to
  regenerate. Cases: minimal app; app with a database and a domain; app with a
  volume and `replicas: 1`; a case with `replicas: 2` and a volume (expected
  error); a case with a secret ref. Fuzz `Parse` like the existing
  `manifest_fuzz_test.go`.
- **Golden tests, compose import.** `testdata/compose/*.yml` with
  `*.shoal.yaml` and `*.warnings.txt`. One case per row of the table in
  section 3, including a postgres service, a redis service, a bind mount,
  `depends_on`, and an unsupported key. `--strict` is tested for exit code `1`.
- **init dry-run test.** `shoal init --dry-run --root-dir $TMP` with
  `SHOAL_INIT_FAKE_HOST=testdata/init/host-1g.json`. The output is compared
  with a golden file. The test also asserts that the tree under `$TMP` is
  unchanged and that no socket is opened (the job runs under `unshare -n`).
- **init idempotence test.** Two runs under `--root-dir` with fake probes. The
  second run prints only `ok` lines and exits `0`.
- **Failure-path tests.** Each preflight check has a fake-host fixture that
  makes it fail (exit `2`, no writes).
- **E2E on a disposable runner.** The job creates a fresh VM or runner, marks
  it with `/etc/shoal-disposable`, and runs: `shoal init --email ... --accept-tos`
  against the pebble directory (the harness from ADR 0015 pass D, documented in
  `docs/testing.md`); deploys an example app with `database: postgres` and a
  domain resolved through `pebble-challtestsrv`; requests
  `curl --cacert <pebble root> --resolve blog.test:443:<edge IP> https://blog.test/`
  and asserts a 200 and a row written to and read back from Postgres; runs
  `shoal app delete blog`, asserts that no object with `app=blog` remains and
  that `blog-uploads` is kept; then runs `shoal uninstall` and asserts that the
  units and binaries are gone.
- **The shared VPS is never used.** Not for tests, benchmarks or e2e of this
  ADR. The `codex-vps` alias and `~/src/shoal-*` are not targets of any job. The
  disposable marker check (section 5) enforces this in code.

### 7. Implementation passes

Each pass is one agent session. A is the base for B, C and D. E is independent
of B to D except for the shared token and join code, so it can start after A.

- **A. App spec library and golden tests.** `internal/appspec` with `Parse`,
  `Validate`, `Expand`, `Diff`; the additive `ObjectMeta.labels` proto change
  (2.4); golden and fuzz tests (6). Volume and Database expansion is generated
  and tested, and is refused by deploy with a clear message until ADR 0016 has
  landed the kinds.
- **B. Deploy and app commands.** `shoal deploy`, `app status`, `app logs`,
  `app delete`, `app secret set` (2.5); the wait logic; the summary output;
  the fix to `examples/route.yaml`. **Done (pass B).** Code: `internal/deploy`
  (plan, batch apply, prune, wait, delete, secret set; tested against an
  in-memory ControlClient), with `cmd/shoal/deploy.go` and `cmd/shoal/app.go`
  as thin wrappers. Differences from the text above, as built:
  - Flags: `--wait` (default 5m, 0 skips) replaces `--timeout`; `--prune` and
    `--prune-data` replace `--delete-volumes`; `deploy` has no confirmation
    prompt (the plan is printed, and the apply runs).
  - A timed-out wait exits 1 with the pending objects and their messages, not
    exit code 3. Exit 3 needs the E-code work of pass D.
  - Labels are only `app=<name>`. `shoal.dev/managed-by` is not set yet.
  - `app delete` keeps the Secret with the data objects. `--data` deletes
    Volumes, the Database and the Secret, and prompts unless `--yes`.
  - `app logs` has no `--db` or `--replica`. Lines are always prefixed with the
    replica name.
  - The service address is taken as `<app>.shoal:80`; this is not yet checked
    against the Service's real DNS name.
  - Deploy does not check the server version for label support (2.4), and it
    has no E-codes (pass D).
  - Not verified against real containers, the edge, or the Database controller.
    Those run in the e2e pass (F).
- **C. Compose import.** `shoal import compose` (3), with the golden and
  `--strict` tests.
- **D. Error catalogue.** `internal/errs` with the 15 codes and hints (4); wired
  into deploy, status and the portal banner.
- **E. Init, uninstall and join.** Preflight, sysctl, containerd and CNI
  install with pins (1.2 to 1.3); the systemd units; `token create --bundle`;
  `agent --token-file`; `--dry-run`, idempotence and failure-path tests; uninstall
  (1.7).
- **F. E2E and benchmark.** The disposable-runner e2e (6), `scripts/bench/onboard.sh`
  and the `onboard-bench` CI job (5), and `docs/benchmarks/onboarding.csv`.
  **Status: partly done.** The deploy part of the e2e (6) is verified in
  `deploy-e2e.yml` (run 38141941992, commit 2b223f9, `docs/testing.md`): a
  `shoal.yaml` with `database: postgres`, a volume and a domain is deployed,
  the app connects to Postgres over HTTPS, rows survive a Postgres task kill,
  a second deploy is unchanged, `app status` is healthy, and `app delete` keeps
  the Database and the Volume. Not done: `shoal init` and `shoal uninstall` in
  that e2e (the e2e starts the server and agent directly, as the ingress e2e
  does), the `curl --resolve` step with `pebble-challtestsrv` (the e2e maps the
  host in `/etc/hosts`), `scripts/bench/onboard.sh`, the `onboard-bench` job and
  `docs/benchmarks/onboarding.csv`. Section 6 also asks that no object with
  `app=blog` remains after `app delete`. That conflicts with keeping
  `blog-uploads` (section 2.5), so the e2e asserts the kept Database and Volume
  instead.

## Consequences
- The 5-minute path is one file and three commands: `init`, `deploy`, and the
  DNS record. The server and agent stay generic, and the app layer is a library.
- Ownership by label needs a proto change, and an older server silently drops
  labels. Deploy must refuse on such a server rather than deploy unowned objects.
- Shoal owns containerd updates through its pins. This is a real maintenance
  cost, accepted in exchange for one install path.
- `database: postgres` on a 1 GiB host is tight. The 384Mi default and a small
  Postgres configuration make it possible, but a heavier workload needs a
  bigger host, and the error catalogue (E09) says so.
- Compose import is a convenience and not a full compatibility layer. Its
  output says what it dropped, so nothing is lost silently.

## Open questions and decisions
Accepted with this ADR. Each question keeps its original text, and the decision
follows it.

1. **Labels versus name-only ownership.** This ADR adds ObjectMeta.labels.
   The alternative is a naming convention plus an owner field. Labels are
   more general, but they widen the proto change.
   *Decision:* labels. ObjectMeta.labels (field 7) landed in ce58ce4. Older
   servers drop it, so deploy refuses labelled objects on them (section 2.4).
2. **uild: in v1.** This ADR rejects it and expects images from CI. Whether
   Shoal should build images (for example with buildkit) is a separate decision.
   *Decision:* uild: stays rejected in v1 with the hint to push an image.
   Image building is deferred to the git-builds roadmap item.
3. **Pull secrets.** E01 needs image_pull_secret on WorkloadSpec. The field
   needs its own pass, and the ADR 0011 Secret model should be checked against it.
   *Decision:* the image pull secret field is added later, in pass D.
4. **Several edges and DNS.** Joining a second edge does not give failover; DNS
   must list both A records. Whether init should print the record set for the
   whole cluster needs a decision.
   *Decision:* multi-edge DNS comes later. Joining a second edge does not change
   DNS, which stays an operator step.
5. **Containerd pin and verification.** Which 2.x release is pinned, and whether
   to verify cosign or GitHub attestations in addition to SHA-256.
   *Decision:* pin the latest containerd 2.x release with its SHA-256. Cosign
   verification comes later.
6. **Sysctl and loop devices.** Init writes sysctl and loads r_netfilter by
   default. Confirm that operators accept this, and whether --volume-backend=dir
   should be the default on hosts without /dev/loop-control.
   *Decision:* init may load r_netfilter and write the required sysctls. The
   dry-run shows each of them before any write.
7. **Database memory default.** 384Mi on a 1 GiB host is an expansion-level
   choice. ADR 0016 section 3.1 still says 2Gi, and that needs an update.
   *Decision:* the database: postgres default is 384Mi memory and 500m CPU.
   ADR 0016 section 3.1 is updated to match in pass C.
8. **Credential Secret name and key.** log-db with key url is assumed from
   ADR 0016 section 3.2. Confirm it in pass C.
   *Decision:* the credentials Secret is <db>-credentials, key url. For an
   app named log, that is log-db-credentials. Section 2.2 is read with this name.
9. **--accept-tos as a flag on init.** Alternatively a one-time prompt on a
   TTY. The flag is simpler to script and is proposed here.
   *Decision:* the terms of service are accepted with the --accept-tos flag.
   There is no interactive prompt.
10. **Compose depends_on.** Ignored in v1. Whether to add a start-order hint
    to the Workload spec is later work.
    *Decision:* a warning only in v1. No start-order hint is added.