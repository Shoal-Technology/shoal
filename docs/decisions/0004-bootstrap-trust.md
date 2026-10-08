# 0004: Bootstrap trust

Status: Accepted (implemented in Phase 2)

## Context
A new node must trust the server, and the server must trust the node, before
any certificates exist on the node.

## Decision
- On first start the server generates an ECDSA P-256 CA, a server cert, and an
  admin client cert (`<data-dir>/admin.conf`).
- `shoal token create` prints `--token <id>.<secret> --ca-hash sha256:<hex>`.
  Only the secret's hash is stored. Tokens expire (default 24h) and can have a
  usage limit.
- The agent connects with TLS and verifies the server CA against the pinned
  hash, then calls `Join` with the token and a CSR. The private key is
  generated on the node and never leaves it.
- Node certs are valid for 30 days and renewed at two-thirds of their lifetime.
  Node identity comes from the cert, never from request payloads.
- TLS 1.3 only and no plaintext listener. Validity checks use server time, and
  certs are backdated 5 minutes to tolerate clock skew.

## Consequences
Same model as kubeadm. Possession of a token plus network access is enough to
join until the token expires. The CA key sits on the server's disk (0600).
