# Quality and security gate

The `checks` job in `.github/workflows/ci.yml` runs on every push and pull
request. This page says what it checks and what it found the last time it was
run by hand.

## What the gate checks

| Step | What it proves | Tool and pin |
| --- | --- | --- |
| Test report parser | The JSON report helper still works | `python3 -m unittest` |
| gofmt | Every Go file is gofmt-clean | `gofmt -l .` must print nothing |
| Vet | Default and `integration` build tags vet cleanly | `make vet` |
| Race tests | Unit tests pass with the race detector. Fuzz seed corpora run as part of this step. | `go test -race ./...` |
| Build CLI | The CLI builds and runs `--help` | `make build` |
| linux/arm64 build | The CLI cross-compiles for arm64 with CGO off | `go build` with `GOOS=linux GOARCH=arm64` |
| Integration compile | The real-runtime suite still compiles | `go test -c -tags integration` |
| staticcheck | No static analysis findings | `honnef.co/go/tools` v0.8.1, built with `golang.org/x/tools` v0.52.0 |
| govulncheck | No known vulnerability is reachable from the code | `golang.org/x/vuln/cmd/govulncheck` v1.2.0 |

The Go toolchain is set by `go` in `go.mod` (currently 1.27.3), so CI and the
VPS build with the same patched standard library.

### Why staticcheck is built the way it is

staticcheck v0.8.1 bundles an older `golang.org/x/tools`. That version cannot
read the export data that Go 1.27.2 writes for the standard library, and fails
with `export data version 5 is greater than maximum supported version 4`. The
CI step therefore builds staticcheck in a throwaway module that pins
`golang.org/x/tools` v0.52.0. Bump this pin together with the staticcheck
version.

## Fuzz tests

Each target has a seed corpus in the test file, and `go test` runs those
seeds on every normal test run. The targets were also run for 25 seconds each
with `go test -fuzz`:

| Target | Package | Input | Property checked |
| --- | --- | --- | --- |
| `FuzzDecode` | `internal/resource` | Manifest YAML (`shoal apply -f`) | Never panics. An accepted manifest yields only objects with an inner resource. |
| `FuzzRouteHost` | `internal/resource` | Route host | An accepted host is lowercase, at most 253 bytes, not an IP, has at least two labels, and each label is a valid DNS label. |
| `FuzzDecode` | `internal/backup` | Backup file | Never panics. An accepted backup survives an Encode and Decode round trip with the same revision and records. |
| `FuzzParseJoinBundle` | `internal/server` | Join bundle | Never panics. An accepted bundle has a PEM CA certificate whose hash matches and non-empty keys. |

Results today: all four passed for the full 25 seconds with no crashers, so
no crasher was saved to a `testdata/fuzz` directory. The seeds are the
`f.Add` entries in each test file.

## Findings and fixes (govulncheck, 2026-10-11)

Before the fix, govulncheck found 26 vulnerabilities that the code reaches:

- **Standard library (Go 1.27.1):** GO-2026-6599, 6600, 6604, 6605, 6607,
  6608, 6609, 6613. Fixed by moving the toolchain to Go 1.27.2 (`go` in `go.mod`).
- **golang.org/x/net v0.58.0:** GO-2026-6603, 6610, 6611, 6612, 6617.
  Fixed by x/net v0.60.0.
- **google.golang.org/grpc v1.76.0:** GO-2026-4762, 6061, 6348, 6443.
  Fixed by grpc v1.83.2.
- **github.com/containerd/containerd/v2 v2.2.1:** GO-2026-5064, 5338, 5378,
  5475, 5622, 5758, 6444, 6597. Fixed by containerd v2.2.9.
- **go.opentelemetry.io/otel v1.37.0:** GO-2026-5506. Fixed by otel v1.44.0,
  which grpc v1.83.2 requires.

Also bumped, not called by the code: `github.com/klauspost/compress` to
v1.18.7 (GO-2026-5841).

After the fix, govulncheck reports 0 vulnerabilities in code the repository
reaches.

### Accepted, not fixed

- **GO-2026-5932, golang.org/x/crypto v0.57.0.** No fixed version exists yet.
  govulncheck says the code does not call the affected code, so it exits 0.
  No ignore was added, because the scanner does not need one. Revisit it when
  a fixed version is released.

### No false positives

staticcheck reported nothing, so no ignore was added.

## Other changes

- `go` in `go.mod` moved from 1.27.1 to 1.27.2, so `setup-go` in CI picks up
  the patched standard library.
- The `checks` job timeout went from 15 to 20 minutes to fit the added steps.

## Last run on the VPS

Commands ran on the shared VPS with Go 1.27.2, without the shoal binary and
without integration tests:

- `gofmt -l .`: clean.
- `go build ./...`, `go vet ./...`, `go vet -tags integration ./...`: clean.
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/shoal`: clean.
- `go test -race -count=1 ./...`: all packages pass.
- staticcheck v0.8.1 (x/tools v0.52.0): no findings.
- govulncheck v1.2.0: 0 reachable vulnerabilities.

CI results are in the commit history and the GitHub Actions run linked from
the pull request or push.
