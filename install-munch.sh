#!/bin/sh
#
# install-munch.sh — install the latest stable munch CLI.
#
# Usage: sh install-munch.sh
#
# Resolves the newest published munch-vX.Y.Z release of the
# hndrxc/tokenmunchers repo, verifies its checksum, and atomically installs
# the munch binary into "${MUNCH_INSTALL_DIR:-$HOME/.local/bin}". Nothing
# else is touched; run "munch install" afterwards to set up the service.

set -eu

die() {
    echo "error: $*" >&2
    exit 1
}

[ "$#" -eq 0 ] || die "usage: sh install-munch.sh"

# --- dependencies -------------------------------------------------------------
for tool in curl python3 tar mktemp awk; do
    command -v "$tool" >/dev/null 2>&1 || die "required command not found: $tool"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    die "neither sha256sum nor shasum is available"
fi

# --- platform ------------------------------------------------------------------
case "$(uname -s)" in
    Linux)  goos=linux ;;
    Darwin) goos=darwin ;;
    *) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
    x86_64|amd64)  goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

# --- setup / cleanup -------------------------------------------------------------
dest=${MUNCH_INSTALL_DIR:-"$HOME/.local/bin"}
umask 077
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/munch-install.XXXXXX")
staging=
trap 'rm -rf "$tmpdir"; if [ -n "$staging" ]; then rm -rf "$staging"; fi' EXIT
trap 'exit 1' HUP INT TERM

# --- resolve latest stable release (list API, never /releases/latest) ------------
api=https://api.github.com/repos/hndrxc/tokenmunchers/releases
page=1
while :; do
    json=$(curl -fsSL --proto '=https' --proto-redir '=https' \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" "$api?per_page=100&page=$page") \
        || die "cannot fetch release list from GitHub API"
    rc=0
    tag=$(printf '%s\n' "$json" | python3 -c '
import json, re, sys

pat = re.compile(r"munch-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
try:
    releases = json.load(sys.stdin)
except (ValueError, UnicodeError):
    sys.exit(3)
if not isinstance(releases, list):
    sys.exit(3)
for rel in releases:
    if rel.get("draft") or rel.get("prerelease"):
        continue
    name = rel.get("tag_name", "")
    if pat.fullmatch(name):
        print(name)
        sys.exit(0)
sys.exit(2 if not releases else 1)
') || rc=$?
    case "$rc" in
        0) break ;;
        1) page=$((page + 1)) ;;
        2) die "no stable munch release found" ;;
        *) die "invalid response from GitHub API" ;;
    esac
done
version=${tag#munch-v}
archive="munch-$version-$goos-$goarch.tar.gz"
base=https://github.com/hndrxc/tokenmunchers/releases/download/$tag

# --- download + verify --------------------------------------------------------------
curl -fsSL --proto '=https' --proto-redir '=https' "$base/$archive" -o "$tmpdir/$archive" || die "download failed: $archive"
curl -fsSL --proto '=https' --proto-redir '=https' "$base/SHA256SUMS" -o "$tmpdir/SHA256SUMS" || die "download failed: SHA256SUMS"

expected=$(awk -v archive="$archive" '
    $2 == archive { count++; hash = $1 }
    END {
        if (count != 1 || length(hash) != 64 || hash ~ /[^0-9a-f]/) exit 1
        print hash
    }
' "$tmpdir/SHA256SUMS") || die "expected exactly one valid checksum for $archive"

if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum < "$tmpdir/$archive") || die "cannot hash $archive"
else
    actual=$(shasum -a 256 < "$tmpdir/$archive") || die "cannot hash $archive"
fi
[ "${actual%% *}" = "$expected" ] || die "checksum mismatch for $archive"

# --- extract to staging, atomic install ---------------------------------------------------
case "$dest" in /*) ;; *) dest="./$dest" ;; esac
[ ! -d "$dest/munch" ] || die "install target is a directory: $dest/munch"
mkdir -p "$dest" || die "cannot create directory: $dest"
staging=$(mktemp -d "$dest/.munch-install.XXXXXX")
tar -xOzf "$tmpdir/$archive" munch > "$staging/munch" || die "extraction failed: $archive"
[ -s "$staging/munch" ] || die "empty binary in $archive"
chmod 755 "$staging/munch"
mv -f "$staging/munch" "$dest/munch"

echo "munch $version installed to $dest/munch"
echo "run 'munch install' next to set up the service"
echo "(make sure $dest is on your PATH)"
