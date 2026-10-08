# 0007: Identity and roles in certificates

Status: Accepted (Phase 2)

## Context
ADR 0004 decides how nodes get certificates. The server also has to know
what each caller may do, and clients have to verify the server, whose
address may be an IP, a DNS name or a NAT address the server doesn't know.

## Decision
- **Identity in the subject.** CN is the name (node name, or `admin`), O is
  the role: `shoal:node` or `shoal:admin`. Node certificates are issued from the
  node's CSR, but the subject always comes from the server (the join request's
  node name), never from the CSR.
- **Roles per service**, checked by one gRPC interceptor: `Join` needs no
  certificate, `Control` needs `shoal:admin`, `Agent` needs `shoal:node`. Unknown
  services are denied. Certificates past their expiry are refused even on a
  connection that is still open.
- **Fixed server name.** The server certificate's only SAN is the DNS name
  `shoal-server`, and every client verifies against that name whatever
  address it dials. This is safe because the CA is private to the cluster and
  only ever issues one kind of server certificate; client certificates have
  only the client-auth usage, so a node can't pose as the server. The server
  certificate is issued in memory at every start and never stored.
- **Revocation by deletion.** There is no CRL. Deleting a Node object makes
  `Sync` and certificate renewal refuse that node, so its certificate is
  useless; it must join again with a new token.
- **Admin credentials** (`<data-dir>/admin.conf`, 0600) hold the server
  address, CA, an admin certificate valid for one year and its key. The
  server re-issues them at start when they expire within 30 days or the
  advertised address changed.

## Consequences
No SAN configuration and no certificate changes when the server's IP changes.
Anyone with the admin key is a full admin (no RBAC, a non-goal). A stolen
node key is usable until the Node is deleted or the certificate expires (at
most 30 days).
