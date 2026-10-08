# 0013: Web portal

Status: Accepted (Phase 5; requested by the user, overriding the original
"no web UI" non-goal)

## Context
Operating the cluster should be possible by clicking: an OpenShift-style
portal with full management, without giving up the single binary or adding
a JavaScript build chain.

## Decision
- **Built into the server**, served over HTTPS on `--ui-listen` (default
  `:7444`; empty disables it). Plain HTML, CSS and JavaScript are embedded
  with `go:embed`: no framework, no Node.js or npm, nothing loaded from the
  internet.
- **Same code paths as the CLI.** The JSON API (`/api/...`) calls the Control
  API in-process: validation, secret redaction and rolling updates behave
  exactly as with `shoal`.
- **Login.** One `admin` user with a password stored as a bcrypt hash in the
  state store (so it will replicate in Phase 6). On first start the server
  generates a random password into `<data-dir>/ui-initial-password` (0600).
  It is never logged ("no secrets in logs"). `shoal ui reset-password` sets a
  new one, deletes that file and ends all sessions.
- **Sessions** are random 256-bit tokens held only as SHA-256 hashes in
  server memory, sent as an `HttpOnly; Secure; SameSite=Strict` cookie, valid
  12h and extended on use. A server restart logs everyone out.
- **Hardening.**
  - Every change needs the `X-Shoal-Request` header, which cross-site
    requests can't set (CSRF).
  - After 5 failed logins in 5 minutes an IP is refused for a while, and each
    failure waits 500ms.
  - Strict CSP (`script-src 'self'`, no inline scripts or styles),
    `X-Frame-Options: DENY`, `nosniff`, no referrer.
  - The UI builds all content with DOM calls, never `innerHTML`.
- **Certificate.** Issued from the cluster CA for the server's names and
  addresses (plus `--ui-name`). Browsers warn until the CA, downloadable at
  `/ca.crt`, is trusted; or use `--ui-cert/--ui-key`.
- **Pages:** overview with "needs attention", nodes (add node shows a ready
  join command), workloads (form or YAML creation, scale, restart, edit YAML,
  delete, replicas, rollout state), live logs, services, secrets and configs
  (key-level editing; secret values are write-only), apply YAML.

## Consequences
Full admin access through a password, as with the CLI's admin certificate:
protect the portal port (firewall, or `--ui-listen 127.0.0.1:7444` plus an
SSH tunnel). There is a single user and no RBAC, which remains a non-goal.
Verified in headless Chromium (Playwright) against a real single-node
cluster: every page, the create form, scaling, add node, secret edits and
deletion, with no console or CSP errors.
