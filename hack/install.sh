#!/bin/sh
# install.sh installs the shoal CLI from GitHub Releases on Linux.
#
#   curl -fsSL https://shoal-technology.solutions/install.sh | sh
#   curl -fsSL https://shoal-technology.solutions/install.sh | sh -s -- --dry-run
#
# Environment:
#   SHOAL_VERSION        release tag to install, e.g. v1.2.3 (default: latest release)
#   SHOAL_INSTALL_DIR    install directory (default: /usr/local/bin)
#   SHOAL_DOWNLOAD_BASE  base URL that serves <tag>/<file> for release assets
#                        (default: https://github.com/Shoal-Technology/shoal/releases/download)
#   SHOAL_API_BASE       GitHub API base used to find the latest release
#                        (default: https://api.github.com/repos/Shoal-Technology/shoal)
#
# Flags:
#   --dry-run            print what would be done and change nothing
#   -h, --help           show this help
#
# The script downloads the archive and checksums.txt, checks the sha256 and,
# when cosign is installed, verifies the keyless signature on checksums.txt.
set -eu

REPO_API="${SHOAL_API_BASE:-https://api.github.com/repos/Shoal-Technology/shoal}"
DOWNLOAD_BASE="${SHOAL_DOWNLOAD_BASE:-https://github.com/Shoal-Technology/shoal/releases/download}"
INSTALL_DIR="${SHOAL_INSTALL_DIR:-/usr/local/bin}"
COSIGN_IDENTITY_RE='^https://github\.com/Shoal-Technology/shoal/\.github/workflows/release\.yml@refs/tags/v.+$'
COSIGN_ISSUER='https://token.actions.githubusercontent.com'

DRY_RUN=0

die() {
	printf 'install.sh: error: %s\n' "$1" >&2
	exit 1
}

note() {
	printf 'install.sh: note: %s\n' "$1" >&2
}

info() {
	printf 'install.sh: %s\n' "$1"
}

usage() {
	cat <<'EOF'
Usage: install.sh [--dry-run]

Installs the shoal CLI to /usr/local/bin (sudo is used when needed).
Set SHOAL_VERSION=vX.Y.Z to pick a release; the default is the latest release.
After installing, run: sudo shoal init
EOF
}

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument: $arg (try --help)" ;;
	esac
done

# Platform checks. shoal ships Linux binaries only.
os="$(uname -s)"
[ "$os" = "Linux" ] || die "unsupported OS: $os (shoal releases are for Linux)"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"

# Resolve the release tag.
if [ -n "${SHOAL_VERSION:-}" ]; then
	tag="$SHOAL_VERSION"
else
	tag="$(curl -fsSL "$REPO_API/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
	if [ -z "$tag" ]; then
		# /releases/latest skips pre-releases; while only alpha builds exist, take the newest published one.
		tag="$(curl -fsSL "$REPO_API/releases?per_page=10" 2>/dev/null |
			sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
		[ -z "$tag" ] || note "no stable release yet; installing pre-release $tag"
	fi
	[ -n "$tag" ] || die "could not read the latest release; set SHOAL_VERSION=vX.Y.Z"
fi
case "$tag" in
v*) ;;
*) tag="v$tag" ;;
esac
case "$tag" in
*[!A-Za-z0-9._+-]*) die "invalid version: $tag" ;;
esac

version="${tag#v}"
archive="shoal_${version}_linux_${arch}.tar.gz"
release_url="$DOWNLOAD_BASE/$tag"

# Install directory: use it directly when writable (or when we are root),
# otherwise use sudo.
SUDO=""
if [ "$(id -u)" -eq 0 ]; then
	[ "$DRY_RUN" -eq 1 ] || mkdir -p "$INSTALL_DIR" || die "could not create $INSTALL_DIR"
else
	if [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
		:
	elif mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -w "$INSTALL_DIR" ]; then
		:
	else
		command -v sudo >/dev/null 2>&1 || die "$INSTALL_DIR is not writable and sudo is not installed"
		SUDO="sudo"
	fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
	info "dry run: nothing will be downloaded or installed"
	info "platform:      linux/$arch"
	info "version:       $tag"
	info "archive:       $release_url/$archive"
	info "checksums:     $release_url/checksums.txt"
	if command -v cosign >/dev/null 2>&1; then
		info "signature:     $release_url/checksums.txt.sigstore.json (cosign found, will verify)"
	else
		info "signature:     not checked (cosign not installed)"
	fi
	info "install to:    $INSTALL_DIR/shoal${SUDO:+ (using sudo)}"
	exit 0
fi

tmp="$(mktemp -d)" || die "could not create a temporary directory"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

info "downloading shoal $tag for linux/$arch"
curl -fsSL -o "$tmp/$archive" "$release_url/$archive" ||
	die "download failed: $release_url/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$release_url/checksums.txt" ||
	die "download failed: $release_url/checksums.txt"

# Verify the sha256 of the archive against checksums.txt.
expected="$(awk -v f="$archive" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
else
	die "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || die "sha256 mismatch for $archive (expected $expected, got $actual)"
info "sha256 verified"

# Verify the cosign signature on checksums.txt when cosign is available.
if command -v cosign >/dev/null 2>&1; then
	curl -fsSL -o "$tmp/checksums.txt.sigstore.json" "$release_url/checksums.txt.sigstore.json" ||
		die "download failed: $release_url/checksums.txt.sigstore.json"
	cosign verify-blob \
		--bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "$COSIGN_IDENTITY_RE" \
		--certificate-oidc-issuer "$COSIGN_ISSUER" \
		"$tmp/checksums.txt" >/dev/null 2>&1 ||
		die "cosign signature check failed for checksums.txt"
	info "cosign signature verified"
else
	note "cosign is not installed, so the release signature was not checked (sha256 was verified)"
fi

# Extract the binary and check that it is there.
mkdir "$tmp/extract"
tar -xzf "$tmp/$archive" -C "$tmp/extract" shoal ||
	die "could not extract shoal from $archive"
[ -f "$tmp/extract/shoal" ] || die "archive $archive does not contain shoal"

# shellcheck disable=SC2086
${SUDO} install -m 0755 "$tmp/extract/shoal" "$INSTALL_DIR/shoal" ||
	die "could not install to $INSTALL_DIR/shoal"
info "installed $INSTALL_DIR/shoal ($tag)"

echo
info "next step: sudo shoal init"
