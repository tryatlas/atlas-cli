#!/bin/sh
# Install the atlas CLI from a GitHub release, verifying its sha256.
#
#   curl -fsSL https://github.com/tryatlas/atlas-cli/releases/latest/download/install.sh | sh
#
# Environment:
#   ATLAS_VERSION      a tag such as v1.0.0 (default: the latest release)
#   ATLAS_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   ATLAS_BASE_URL     release download base (default: GitHub; for mirrors and tests)
set -eu

repo="tryatlas/atlas-cli"
dir="${ATLAS_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "atlas install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s): linux and macOS only" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac
if [ "$os/$arch" = "darwin/amd64" ]; then
  fail "no release build for Intel Macs; build from source: make build"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q "$1" -O "$2"; }
else
  fail "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

version="${ATLAS_VERSION:-}"
if [ -z "$version" ]; then
  fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" || fail "could not find the latest release"
  version="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n1)"
  [ -n "$version" ] || fail "could not read the latest release tag"
fi
plain="${version#v}"
base="${ATLAS_BASE_URL:-https://github.com/$repo/releases/download/$version}"
archive="atlas_${plain}_${os}_${arch}.tar.gz"

echo "Downloading atlas $version for $os/$arch…"
fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || fail "$archive is not in checksums.txt"
got="$(sha "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (got $got, want $want)"

tar -xzf "$tmp/$archive" -C "$tmp" atlas
mkdir -p "$dir"
install -m 0755 "$tmp/atlas" "$dir/atlas"
echo "Installed $("$dir/atlas" --version 2>/dev/null || echo "atlas $version") to $dir/atlas"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH to run atlas." ;;
esac
