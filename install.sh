#!/bin/sh
# Install the atlas-cli binary from a GitHub release, verifying its sha256.
#
#   curl -fsSL https://github.com/tryatlas/atlas-cli/releases/latest/download/install.sh | sh
#
# While the repository is private, anonymous downloads fail: the script then
# uses the GitHub CLI (`gh`, signed in with access to the repository), which
# handles the authentication. It never reads or prints a token.
#
# Environment:
#   ATLAS_VERSION      a tag such as v1.0.0 (default: the latest release)
#   ATLAS_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   ATLAS_BASE_URL     release download base (default: GitHub; for mirrors and tests)
set -eu

repo="tryatlas/atlas-cli"
dir="${ATLAS_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "atlas-cli install: $*" >&2; exit 1; }

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

# use_gh: the GitHub CLI is signed in and can see the repository (needed
# while it is private). ATLAS_BASE_URL always means a plain download.
use_gh=no
if [ -z "${ATLAS_BASE_URL:-}" ] && command -v gh >/dev/null 2>&1 && gh release list -R "$repo" --limit 1 >/dev/null 2>&1; then
  use_gh=yes
fi

version="${ATLAS_VERSION:-}"
if [ -z "$version" ]; then
  if [ "$use_gh" = yes ]; then
    version="$(gh release view -R "$repo" --json tagName --jq .tagName 2>/dev/null || true)"
  else
    fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" 2>/dev/null &&
      version="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n1)"
  fi
  [ -n "$version" ] || fail "could not find the latest release (a private repository needs the GitHub CLI: gh auth login)"
fi
plain="${version#v}"
archive="atlas-cli_${plain}_${os}_${arch}.tar.gz"

echo "Downloading atlas-cli $version for $os/$arch…"
if [ "$use_gh" = yes ]; then
  gh release download "$version" -R "$repo" -D "$tmp" -p "$archive" -p checksums.txt ||
    fail "download failed: $archive from $repo $version"
else
  base="${ATLAS_BASE_URL:-https://github.com/$repo/releases/download/$version}"
  fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive (a private repository needs the GitHub CLI: gh auth login)"
  fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"
fi

want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || fail "$archive is not in checksums.txt"
got="$(sha "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (got $got, want $want)"

tar -xzf "$tmp/$archive" -C "$tmp" atlas-cli
mkdir -p "$dir"
install -m 0755 "$tmp/atlas-cli" "$dir/atlas-cli"
echo "Installed $("$dir/atlas-cli" --version 2>/dev/null || echo "atlas-cli $version") to $dir/atlas-cli"
# Releases up to v0.4.0 named the binary atlas; drop it so only one copy remains.
if [ -f "$dir/atlas" ] && "$dir/atlas" --version 2>/dev/null | grep -q '^atlas '; then
  rm -f "$dir/atlas" && echo "Removed the old $dir/atlas (the command is now atlas-cli)."
fi
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH to run atlas-cli." ;;
esac
