# 0011: Secrets and Configs

Status: Accepted (Phase 5)

## Context
Workloads need configuration and credentials, as environment variables or
files. Secrets must be encrypted at rest and never leak through logs or the
API.

## Decision
- **Kinds.** `Secret` and `Config` each hold `data: {key: value}` (at most
  256 KiB per object, keys usable as file names). They are applied like any
  object (`shoal apply`, the portal).
- **Encryption at rest.** The server wraps its store in an
  `EncryptingStore`: Secret specs are sealed with AES-256-GCM and a random
  nonce. Each sealed value carries a key ID, which gives a clear error on a
  wrong key. The record's kind/name is bound in as additional data, so a
  ciphertext can't be moved to another record. The 32-byte key is
  `<data-dir>/secret.key` (0600, created on first start; a key file readable
  by others is refused), overridable with `--encryption-key-file`. It must be
  backed up with the data dir. The wrapper passes the store conformance
  suite, so Raft (Phase 6) gets encryption for free.
- **Never returned.** `get`/`describe` and the portal show a Secret's keys
  with empty values. The portal changes Secrets by setting or removing keys
  only.
- **Use in workloads.** `env[].valueFrom.secret|config: {name, key}` and
  `files: [{path, secret|config}]` (one read-only file per key under `path`).
  - The server resolves references when it builds a node's snapshot, so
    values travel only over mTLS to the nodes that run the replicas.
  - A missing Secret, Config or key is reported as the replica's problem
    (e.g. `secret "db" not found`). The agent then leaves an existing
    container alone and does not start a new one.
- **Changes.** Mounted files are rewritten in place on the next sync, with
  no restart. Environment variables are fixed when the container is created,
  as in Kubernetes: restarting every replica at once on a Secret change would
  cause an outage. The agent therefore hashes env *references*, not values.
  To pick up new values, run `shoal restart workload NAME` (or the portal's
  Restart button): it sets `spec.restartedAt`, which rolls the replicas over
  with the usual rolling update. The same command retries a rolled-back
  release.

## Consequences
Secret values are in plain text in the agents' snapshot cache
(`<state-dir>/agent/assignments.pb`, 0600) and in the mounted files (on the
host under the root-only state dir; mode 0644 inside the container so
non-root apps can read them). Anyone with root on a node can read the
Secrets of the replicas running there, which is inherent to running them.

## Addendum: typed Secrets and image pull secrets (K-28, K-19)

- **Types.** `SecretSpec.type` takes the core/v1 names. An empty type reads as
  `Opaque`, so older Secrets stay valid.
  - `Opaque`: any keys, as before.
  - `kubernetes.io/dockerconfigjson`: the key `.dockerconfigjson` holds a docker
    config whose `auths` map has at least one registry. Each entry gives `auth`
    (base64 of `user:password`) or `username` and `password`. Docker Hub keys
    (`docker.io`, `index.docker.io`, `https://index.docker.io/v1/`) map to
    `registry-1.docker.io`.
  - `kubernetes.io/tls`: `tls.crt` and `tls.key` form a key pair, checked with
    `tls.X509KeyPair`.
  - The server checks the data against the type on every apply
    (`resource.ValidateSecret`). The portal keeps the type when it sets or removes
    keys. An unknown type is refused.
- **Image pull secrets.** `WorkloadSpec.image_pull_secrets` (appspec key
  `imagePullSecrets`) names dockerconfigjson Secrets in the same scope as env
  references. The server resolves them when it builds a node's snapshot, as it
  does for env and files. The logins travel as `Assignment.registry_credentials`
  over mTLS. A missing Secret, a Secret of another type, or one that does not
  parse is the assignment's problem: the replica is reported and an existing
  container is left alone. Error messages name the Secret, never a credential.
- **Use on the node.** The runtime pulls through a containerd resolver that
  logs in per registry host. Other hosts pull anonymously. The pull key holds
  the image, host and user name, not the password. The logins are not part of
  the spec hash, so a new password does not recreate a container; the next pull
  uses it.
- **Consequences.** The logins are plain text in the agent's snapshot cache
  (`assignments.pb`, 0600) and in memory, as Secret values are. Every node that
  runs a replica of the Workload receives the logins for its pull secrets.
  Rotating a registry token means updating the Secret: the agent receives it
  through the watch on Secrets.
- **CLI.** `shoal secret create NAME --type dockerconfigjson --from-docker-config
  FILE`, or `--docker-server`, `--docker-username` and `--docker-password-stdin`
  (the password is never a flag). `--type tls --cert FILE --key FILE`. The data
  is checked locally with the server's rules before connecting.
