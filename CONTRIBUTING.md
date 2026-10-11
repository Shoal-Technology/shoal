# Contributing to Shoal

Thanks for your interest in Shoal. It is an alpha project: the design is
still moving, so please talk to us before starting a large change.

- Security problems: do not open a public issue. See [SECURITY.md](SECURITY.md).
- Conduct: see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
- Bugs and feature ideas: use the issue templates.

## Developer Certificate of Origin (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/)
rather than a Contributor License Agreement. It is lighter: you keep the
copyright to your work, and you confirm that you have the right to submit it
under the Apache License 2.0 that covers this project.

Sign off every commit with `-s`:

```sh
git commit -s -m "server: reject empty route host"
```

This adds a `Signed-off-by: Your Name <you@example.com>` line that must match
the commit author. Maintainers check sign-offs during review, and pull
requests with unsigned commits will be asked to amend them
(`git rebase --signoff origin/main` fixes a whole branch). There is no
separate sign-up.

## Build and test

Requirements: the Go version in `go.mod`, `make`, and `python3` for the
helper scripts under `hack/`.

```sh
make build        # bin/shoal, pure Go, CGO_ENABLED=0
make test         # go test ./...
make race         # go test -race -count=1 ./...
make vet          # go vet, including the integration build tag
```

Before you open a pull request, run at least `make test` and `make vet`. CI
also runs gofmt, shellcheck on `hack/install.sh`, staticcheck, govulncheck,
gitleaks and the dependency license gate (`python3 hack/license_check.py`).

Generated protobuf code is checked in. After editing a `.proto` file under
`api/v1alpha1/`, run `make proto` (it needs `protoc`, `protoc-gen-go` and
`protoc-gen-go-grpc` on your `PATH`) and commit the regenerated files.

### Integration and end-to-end tests

The containerd integration suite (`make integration`, `make integration-report`)
and the end-to-end targets (`make ingress-e2e`, and the workflows in
`.github/workflows/*-e2e.yml`) change host networking, start servers and run
as root. They refuse to run unless `CI=true` (GitHub Actions) or
`SHOAL_DISPOSABLE=1` is set. Only run them on a disposable Linux machine or a
disposable CI runner. Do not run them on a workstation or a shared host.

Multi-host behaviour is not yet verified end to end; see the limitations in
the README before you rely on it.

## Architecture decisions (ADRs)

Decisions that change the design, the on-disk format, the wire protocol, or
the operator's contract are recorded as ADRs in `docs/decisions/`.

1. Copy the format of a recent file, such as `0017-onboarding-init-app-spec.md`.
2. Use the next free four-digit number and a short kebab-case title:
   `docs/decisions/0018-short-title.md`.
3. State the context, the decision, the alternatives considered and the
   consequences. Mark the status as `Proposed` in the pull request.
4. Change the status to `Accepted` when the change merges, or `Superseded by
   NNNN` when a later ADR replaces it. Do not rewrite an accepted ADR's
   decision; write a new one.

Small fixes, refactors and documentation changes do not need an ADR.

## Pull requests

- Keep a pull request to one topic and explain why the change is needed.
- Add or update tests with the change. Bug fixes need a test that fails
  without the fix.
- Update the README and the relevant file under `docs/` when behaviour changes.
- Fill in the pull request template.
- Do not add dependencies lightly. A new dependency must have a license that
  the license gate accepts (Apache-2.0, MIT, BSD, ISC or MPL-2.0), and the
  change must update `NOTICE` when the dependency ships a NOTICE file. Run
  `python3 hack/license_check.py --notice NOTICE` and commit the result.

## Code style

- Run `gofmt` on every Go file. CI fails on unformatted code.
- Keep the module path `github.com/kaiserern/shoal` in imports.
- Shell scripts that run under `sh` must be POSIX; `hack/install.sh` is
  checked with `dash` and `shellcheck`.
