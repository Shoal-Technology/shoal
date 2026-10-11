# Releasing shoal

A release is a `v*` tag. Pushing the tag runs `.github/workflows/release.yml`:
the CI gate (`ci.yml`, including the `install.sh` test), then GoReleaser v2.18.3
builds the archives, checksums, SBOMs and the cosign signature, and creates a
**draft** GitHub Release. Nothing is published until the owner publishes the
draft.

## What a release contains

For each of `linux/amd64` and `linux/arm64`:

- `shoal_<version>_linux_<arch>.tar.gz` with `shoal`, `LICENSE` and `README.md`
- `shoal_<version>_linux_<arch>.tar.gz.sbom.json` (SBOM, Syft JSON)

Plus:

- `checksums.txt` (sha256 of every archive)
- `checksums.txt.sigstore.json` (cosign keyless bundle over `checksums.txt`,
  signed with the GitHub Actions identity of `release.yml` at the tag)

Binaries are built with `CGO_ENABLED=0` (pure Go sqlite, `modernc.org/sqlite`),
so both architectures cross-compile without a C toolchain. The ldflags set
`main.version`, `main.commit` and `main.date`; `shoal version` prints them.

## Before tagging

1. `main` is green in CI, and the last deploy and integration runs you rely on
   are recorded in `docs/testing.md`.
2. Run the snapshot workflow (Actions, "Snapshot", Run workflow). It builds the
   same archives without tagging or publishing, runs the `install.sh` test on
   them, and uploads them as the `shoal-snapshot` artifact. Check the archive
   contents and `shoal version` for amd64 as described below.
3. Pick the version. Use semantic versioning, `vMAJOR.MINOR.PATCH`. Prerelease
   tags such as `v1.2.0-rc.1` are marked prerelease automatically.

## Cutting a release

```sh
git checkout main && git pull --ff-only
git tag -a v1.2.3 -m "shoal v1.2.3"
git push origin v1.2.3
```

Watch the "Release" run. If the gate fails, nothing is built. Fix forward with a
new tag; do not move or reuse a tag that has been pushed.

When the run is green, open the draft release on GitHub, check the asset list
(two archives, `checksums.txt`, `checksums.txt.sigstore.json`, two SBOMs), add
release notes, and publish it. `install.sh` resolves "latest" through the API,
which only sees published releases.

## Checking a release

Run these on a Linux host, after downloading the assets into an empty directory:

```sh
sha256sum -c --ignore-missing checksums.txt
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/kaiserern/shoal/\.github/workflows/release\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
tar -tzf shoal_1.2.3_linux_amd64.tar.gz     # shoal, LICENSE, README.md
```

Then check the install path on a disposable host:

```sh
curl -fsSL https://shoal-technology.solutions/install.sh | sh -s -- --dry-run
SHOAL_VERSION=v1.2.3 sh install.sh && shoal version
```

## Testing a build without a tag

```sh
goreleaser release --snapshot --clean --skip=sign   # needs goreleaser v2.18.3 and syft
bash hack/test_install.sh dist
```

`--skip=sign` is needed locally because cosign keyless signing needs a CI OIDC
token. Snapshot builds never sign.

## Things that are not automated

- The Go module path is `example.com/shoal` and stays that way until the owner
  decides otherwise. Changing it is a separate change.
- The repository is private. `install.sh` cannot download from a private
  repository without a token, so it only works for anonymous users after the
  repository is made public. Making it public is the owner's decision.
- The website copy of `install.sh` (`~/shoal-site/install.sh` on the VPS) is
  updated by hand from `hack/install.sh` when the script changes. Its
  `install.sh.sha256` must be updated at the same time.
