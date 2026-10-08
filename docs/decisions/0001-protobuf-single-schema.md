# 0001: Protobuf as the single schema

Status: Accepted (implemented from Phase 2)

## Context
Resources are written by users as YAML, sent over gRPC and stored. Defining
them separately as Go structs (YAML) and proto messages (API) duplicates the
schema and lets the two drift apart.

## Decision
`.proto` files under `api/v1alpha1` are the only schema. YAML is converted to
JSON and decoded with `protojson`, rejecting unknown fields. Storage uses
binary proto. Quantities stay strings in the API (`"200m"`, `"128Mi"`) and
are parsed by `internal/resource` during validation. Generated code is checked
in.

## Consequences
- One definition and no k8s apimachinery dependency.
- YAML field names follow protojson (lowerCamelCase), which matches the examples.
- Adds `google.golang.org/protobuf`, `grpc` and `sigs.k8s.io/yaml` (small, already used in Phase 1).
