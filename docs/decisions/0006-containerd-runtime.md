# 0006: containerd runtime integration

Status: Accepted (Phase 1; networking revised in Phase 3)

## Context
The agent must run OCI containers through containerd, survive its own
restarts without disrupting containers, keep logs, and give containers
networking with published ports.

## Decision
- **Namespace:** all containers live in a dedicated containerd namespace
  (`shoal`). The agent owns it: anything there that isn't assigned is removed.
- **Labels are the state.** Replica ID, workload, spec hash, start count,
  start time, IP, ports and a stop-requested flag (set by `Stop`, cleared by
  `Start`) are stored as container labels. The flag lets the agent tell
  "stopped because desired was Stopped" from "the app exited", so switching
  back to Running works under every restart policy. The agent keeps no
  local database. A container whose task disappeared (e.g. after a host
  reboot) is reported as exited with code -1, so restart policies apply.
- **Logs:** containerd's *binary logging* mode. The shim runs
  `shoal --shoal-log-driver <file>` per task, which writes timestamped,
  stream-tagged lines with size rotation. Logs survive agent restarts and
  container restarts. No FIFOs are held by the agent.
- **Networking:** CNI via `github.com/containerd/go-cni`: `bridge` +
  `host-local` + `portmap` + `firewall`. Each container gets a **pinned
  network namespace** (`<state-dir>/netns/`, created with containerd's
  `pkg/netns`, recorded in a label). It is set up on first start, kept across
  restarts and torn down with its path on Remove. The container spec's network
  namespace is pointed at it before each start.
  *Revised in Phase 3.* Phase 1 attached the task's own netns (through the
  process's PID) and tore down with an **empty** netns path, because by then
  the process was gone and a stale PID path could belong to another process
  in the host namespace. But with an empty path the bridge plugin skips its
  iptables cleanup, so every removed or restarted container leaked a
  masquerade rule and chain (measured: 23 chains on the test VM). A pinned
  netns exists until we remove it, so teardown is both safe and complete.
  After a host reboot the bind mount (and the iptables rules) are gone; the
  next start notices, releases the IP with an empty path and sets up a new
  netns. If a netns is lost while iptables keeps its rules (state dir wiped,
  or rules leaked by an older version), the leftover rule would block
  deleting the chain and so every later teardown of that container ID. The
  runtime therefore removes the container's masquerade rules itself before
  each fresh setup and on removal without a netns. It matches them by the
  plugin's exact comment (`name: "<network>" id: "<container>"`) and deletes
  them by full spec, not by number.
- **Isolation defaults:** private cgroup namespace, read-only `/sys/fs/cgroup`
  (apps can see their limits), generated `/etc/hosts` and `/etc/resolv.conf`
  (the host's upstream resolvers, since systemd-resolved's 127.0.0.53 is
  unreachable from a container netns).
- **Images:** pulled only when missing (no pull policy yet). Mutable tags are
  not re-pulled.

## Dependencies added
- `github.com/containerd/containerd/v2` (required by the project).
- `github.com/containerd/go-cni`: the standard CNI client, already part of the
  containerd ecosystem. Executing plugins by hand would mean re-implementing
  libcni.
- `github.com/spf13/cobra` (required), `sigs.k8s.io/yaml` (small; strict
  decoding for the standalone file).
- Phase 3: containerd's `pkg/netns` (part of the containerd module) brings in
  `github.com/moby/sys/symlink` and `github.com/containernetworking/plugins`
  (`pkg/ns` only) as indirect dependencies. Both are what containerd's own
  CRI uses for the same purpose.

## Consequences
CNI plugins in `/opt/cni/bin` become a host prerequisite next to containerd.
Changing `--pod-cidr` on a node requires deleting the old bridge; the runtime
detects the mismatch at startup and says so. A container keeps its IP across
restarts, but a replaced replica gets a new one (stable addresses are the
Service's job in Phase 5).
