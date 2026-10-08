# 0012: Services, DNS and the WireGuard mesh

Status: Accepted (Phase 5)

## Context
Replicas get new IPs when they are replaced and run on different nodes.
Clients need a stable address, load balancing over the ready replicas, a
name to find it by, and connectivity between containers on different nodes.

## Decision
- **Container subnets.** The server gives each node its own `/24` from the
  cluster CIDR (`--cluster-cidr`, default `10.250.0.0/16`), on first use via
  the agent's `NodeConfig` call. The agent starts no container before it
  knows its subnet (the answer is cached for server outages). `--pod-cidr`
  only applies to standalone agents.
- **Services.** A `Service` selects a workload and lists ports. The
  controller gives it a virtual IP from `--service-cidr` (default
  `10.96.0.0/16`) and keeps `status.endpoints` at the IPs of the workload's
  *available* replicas (ready on their current template).
- **Load balancing on every node** with iptables in shoal-owned chains,
  replaced atomically with `iptables-restore --noflush`:
  - `SHOAL-SERVICES` (nat PREROUTING/OUTPUT): VIP:port → one per-port chain
    that picks an endpoint uniformly (`statistic random`) and DNATs to it.
  - Service connections are marked and masqueraded (`SHOAL-POSTROUTING`),
    so replies come back through the node that translated them, also when
    client and replica share a bridge.
  - Traffic between container subnets is not masqueraded: pods see each
    other's real IPs.
  - `SHOAL-FORWARD`/`SHOAL-OUTPUT` (filter) **reject** connections to a
    Service without endpoints right away (TCP reset / ICMP). *Found in
    testing:* without that, packets to the VIP went towards the default
    route and clients hung (busybox `wget` for 15 minutes) instead of
    retrying. `SHOAL-FORWARD` also accepts cluster traffic, as Docker sets
    the FORWARD policy to DROP.
  - Chains of removed Services are flushed and deleted, including those left
    by a previous agent process.
- **DNS.** Each agent answers on its bridge address (port 53, UDP; bound
  with `IP_FREEBIND` before the bridge exists). `<service>.shoal` returns the
  VIP (TTL 5s) and unknown `*.shoal` names get NXDOMAIN. Everything else is
  forwarded to the host's resolvers. Containers get
  `nameserver <bridge IP>`, `search shoal`, so `http://web/` works too.
  Built on `golang.org/x/net/dns/dnsmessage` (already a dependency).
- **WireGuard mesh.**
  - Each agent creates an interface (`--wireguard-interface`, default
    `shoal-wg`, empty disables it) on `--wireguard-port` (51820) with its own
    key (`<state-dir>/agent/wireguard.key`, never leaves the node).
  - It announces the public key and port in its heartbeats; the server
    records the endpoint (the address it sees, or `--wireguard-endpoint`
    behind NAT) and sends every node the other nodes as peers.
  - Each peer's subnet is routed into the tunnel. Routes are added, never
    replaced, and peers on the same host are not routed.
  - *Found in testing:* replacing routes hijacked a local bridge's route when
    two agents shared a host, which broke health checks and DNS.
  - Configured through `golang.zx2c4.com/wireguard/wgctrl` (netlink, no `wg`
    tool needed); the interface and routes with `ip`.

## Consequences
New dependencies: `wgctrl` and its netlink helpers (`mdlayher/*`,
`golang.zx2c4.com/wireguard`). Nodes must reach each other on the WireGuard
UDP port. iptables (legacy or nft-backed `iptables`) and the WireGuard kernel
module are host prerequisites for multi-node clusters. On the single test VM,
the mesh setup (keys, peers, routes) was verified, but traffic between two
real hosts through the tunnel was not.
