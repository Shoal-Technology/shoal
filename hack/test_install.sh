#!/usr/bin/env bash
# Tests hack/install.sh against release-shaped archives served over local HTTP.
#
#   bash hack/test_install.sh [DIST_DIR]
#
# DIST_DIR is a goreleaser dist directory holding the linux archive for this
# host plus checksums.txt (CI passes the snapshot output). Without it, the
# test builds shoal from ./cmd/shoal and packages it the way goreleaser does.
# Only the Linux amd64/arm64 hosts are supported, and nothing is installed
# outside a temporary directory.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/hack/install.sh"
work="$(mktemp -d)"
server_pid=""

cleanup() {
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac

fail() {
	echo "FAIL: $1" >&2
	exit 1
}

pass() {
	echo "PASS: $1"
}

# Build or locate the archive and checksums for this host.
release_dir="$work/srv/releases"
mkdir -p "$release_dir"
if [ $# -ge 1 ]; then
	dist="$(cd "$1" && pwd)"
	mapfile -t found < <(cd "$dist" && ls shoal_*_linux_"$arch".tar.gz 2>/dev/null)
	[ "${#found[@]}" -eq 1 ] || fail "expected one shoal_*_linux_$arch.tar.gz in $dist, found ${#found[@]}"
	archive="${found[0]}"
	cp "$dist/$archive" "$release_dir/"
	cp "$dist/checksums.txt" "$release_dir/"
	version="${archive#shoal_}"
	version="${version%_linux_"$arch".tar.gz}"
else
	version="0.0.0-test"
	stage="$work/stage"
	mkdir -p "$stage"
	(cd "$root" && CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$version -X main.commit=testcommit -X main.date=2026-01-01T00:00:00Z" \
		-o "$stage/shoal" ./cmd/shoal)
	cp "$root/LICENSE" "$root/README.md" "$stage/"
	archive="shoal_${version}_linux_${arch}.tar.gz"
	tar -czf "$release_dir/$archive" -C "$stage" shoal LICENSE README.md
	(cd "$release_dir" && sha256sum "$archive" > checksums.txt)
fi
tag="v$version"

# Fake GitHub API for the "latest release" lookup.
mkdir -p "$work/srv/api/releases"
printf '{"tag_name": "%s"}\n' "$tag" >"$work/srv/api/releases/latest"

# Layout matching the release download URLs: <base>/<tag>/<file>.
mkdir -p "$work/srv/dl/$tag"
cp "$release_dir/$archive" "$release_dir/checksums.txt" "$work/srv/dl/$tag/"

# A second base with a tampered archive but the original checksums.
mkdir -p "$work/srv/bad/dl/$tag"
cp "$release_dir/checksums.txt" "$work/srv/bad/dl/$tag/"
cp "$release_dir/$archive" "$work/srv/bad/dl/$tag/"
printf 'tampered' >>"$work/srv/bad/dl/$tag/$archive"

port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work/srv" >"$work/http.log" 2>&1 &
server_pid=$!
for _ in $(seq 1 50); do
	if curl -fsS -o /dev/null "http://127.0.0.1:$port/dl/$tag/checksums.txt" 2>/dev/null; then
		break
	fi
	sleep 0.2
done
base="http://127.0.0.1:$port/dl"
api="http://127.0.0.1:$port/api"

# 1. Dry run changes nothing.
SHOAL_VERSION="$tag" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin-dry" \
	sh "$script" --dry-run >"$work/dry.out" 2>&1 || fail "dry run exited non-zero"
[ ! -e "$work/bin-dry/shoal" ] || fail "dry run installed a binary"
grep -q "$archive" "$work/dry.out" || fail "dry run output does not name the archive"
pass "dry run prints the plan and installs nothing"

# 2. Install with an explicit version (no leading v) into a writable dir.
SHOAL_VERSION="$version" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin" \
	sh "$script" >"$work/install.out" 2>&1 || { cat "$work/install.out"; fail "install failed"; }
[ -x "$work/bin/shoal" ] || fail "binary not installed or not executable"
grep -q "sudo shoal init" "$work/install.out" || fail "next step not printed"
pass "installs with SHOAL_VERSION=$version and prints the next step"

# 3. The installed binary reports the version it was built with.
out="$("$work/bin/shoal" version)"
case "$out" in
"shoal $version (commit "*) pass "shoal version prints: $out" ;;
*) fail "unexpected shoal version output: $out" ;;
esac

# 4. Latest release is resolved through the API when SHOAL_VERSION is unset.
SHOAL_API_BASE="$api" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin-latest" \
	sh "$script" >"$work/latest.out" 2>&1 || { cat "$work/latest.out"; fail "latest-release install failed"; }
[ -x "$work/bin-latest/shoal" ] || fail "latest-release install did not write the binary"
grep -q "$tag" "$work/latest.out" || fail "latest-release install did not report $tag"
pass "resolves the latest release through the API"

# 5. A checksum mismatch must stop the install.
if SHOAL_VERSION="$tag" SHOAL_DOWNLOAD_BASE="http://127.0.0.1:$port/bad/dl" SHOAL_INSTALL_DIR="$work/bin-bad" \
	sh "$script" >"$work/bad.out" 2>&1; then
	fail "tampered archive was accepted"
fi
grep -q "sha256 mismatch" "$work/bad.out" || { cat "$work/bad.out"; fail "no sha256 mismatch reported"; }
[ ! -e "$work/bin-bad/shoal" ] || fail "tampered archive installed a binary"
pass "rejects a tampered archive"

# 5b. With no stable release, the newest published release is used (install.sh
# falls back to the release list, which includes pre-releases).
mkdir -p "$work/srv/api-pre"
printf '[{"tag_name": "%s"}]\n' "$tag" >"$work/srv/api-pre/releases"
SHOAL_API_BASE="http://127.0.0.1:$port/api-pre" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin-pre" \
	sh "$script" >"$work/pre.out" 2>&1 || { cat "$work/pre.out"; fail "pre-release fallback install failed"; }
[ -x "$work/bin-pre/shoal" ] || fail "pre-release fallback did not write the binary"
grep -q "installing pre-release $tag" "$work/pre.out" || fail "pre-release fallback was not reported"
pass "falls back to the newest published release when there is no stable one"

# 5c. With no release at all, the install stops and says why.
if SHOAL_API_BASE="http://127.0.0.1:$port/api-none" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin-none" \
	sh "$script" >"$work/none.out" 2>&1; then
	fail "install without any release was accepted"
fi
grep -q "could not read the latest release" "$work/none.out" || { cat "$work/none.out"; fail "no release not reported"; }
[ ! -e "$work/bin-none/shoal" ] || fail "install without a release installed a binary"
pass "stops with a clear error when no release exists"

# 6. A missing release fails cleanly.
if SHOAL_VERSION="v9.9.9-missing" SHOAL_DOWNLOAD_BASE="$base" SHOAL_INSTALL_DIR="$work/bin-missing" \
	sh "$script" >"$work/missing.out" 2>&1; then
	fail "missing release was accepted"
fi
pass "fails cleanly for a release that does not exist"

# 7. Unknown flags and bad versions are rejected before any download.
if sh "$script" --bogus >/dev/null 2>&1; then fail "unknown flag accepted"; fi
if SHOAL_VERSION='v1/../x' sh "$script" --dry-run >/dev/null 2>&1; then fail "invalid version accepted"; fi
pass "rejects unknown flags and invalid versions"

echo "all install.sh tests passed ($tag, linux/$arch)"
