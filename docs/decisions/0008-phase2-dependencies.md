# 0008: SQLite driver and gRPC dependencies

Status: Accepted (Phase 2)

## Context
Phase 2 needs a SQLite driver without CGO (the binary must stay static), plus
gRPC and protobuf.

## Decision
- **`modernc.org/sqlite`**: SQLite translated to pure Go. It is the most
  widely used CGO-free driver and works with `database/sql`. The alternative,
  `ncruces/go-sqlite3`, runs SQLite as WebAssembly and pulls in a WASM
  runtime. modernc adds about 10 MB to the binary and a few indirect
  `modernc.org/*` modules.
- The store uses **one connection** (`SetMaxOpenConns(1)`), WAL mode and
  `BEGIN IMMEDIATE` transactions: all access is serialized, which is plenty
  for a small cluster and rules out `SQLITE_BUSY`.
- **`google.golang.org/grpc`** and **`google.golang.org/protobuf`**, already
  indirect dependencies through containerd, become direct ones (same versions).
- Code generation uses `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`
  (`make proto`). They are only needed after editing a `.proto` file, since
  the generated code is checked in.

## Consequences
Measured on a 2-vCPU VM: server idle RSS about 25 MB, agent (server mode)
about 21 MB, well within the 100 MB / 50 MB targets.
