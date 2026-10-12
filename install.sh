#!/bin/sh
# Install the atlas-cli binary from a GitHub release, verifying its sha256.
#
#   curl -fsSL https://github.com/tryatlas/cli/releases/latest/download/install.sh | sh
#
# Written for POSIX sh (dash, ash, busybox, bash --posix, zsh in sh mode): no
# mktemp, install or tar -z, which POSIX does not define. Beyond the POSIX
# utilities it needs curl or wget, gzip and tar, and one of sha256sum, shasum,
# sha256 or openssl.
#
# Anonymous downloads fail on a private repository: the script then uses the
# GitHub CLI (`gh`, signed in with access to the repository), which handles
# the authentication. It never reads or prints a token.
#
# Environment:
#   ATLAS_VERSION      a tag such as v1.0.0 (default: the latest release)
#   ATLAS_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   ATLAS_BASE_URL     release download base (default: GitHub; for mirrors and tests)
set -eu

repo="tryatlas/cli"
bin="atlas-cli"
dir="${ATLAS_INSTALL_DIR:-${HOME:?HOME is not set; set ATLAS_INSTALL_DIR}/.local/bin}"

say() { printf '%s\n' "$*"; }
fail() {
  printf '%s install: %s\n' "$bin" "$*" >&2
  exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

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

if has curl; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif has wget; then
  fetch() { wget -q "$1" -O "$2"; }
else
  fail "needs curl or wget"
fi

if has sha256sum; then
  sha() { sha256sum "$1" | cut -d ' ' -f 1; }
elif has shasum; then
  sha() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
elif has sha256; then
  sha() { sha256 -q "$1"; }
elif has openssl; then
  sha() { openssl dgst -sha256 "$1" | sed 's/^.*= *//'; }
else
  fail "needs sha256sum, shasum, sha256 or openssl"
fi

has gzip || fail "needs gzip"
has tar || fail "needs tar"

# A private temporary directory; POSIX has no mktemp, so fall back to mkdir,
# which fails rather than reuse a path that already exists.
tmp="$(mktemp -d 2>/dev/null)" || {
  tmp="${TMPDIR:-/tmp}/$bin-install.$$"
  (umask 077 && mkdir "$tmp") || fail "cannot create a temporary directory"
}
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

# use_gh: the GitHub CLI is signed in and can see the repository (needed
# while it is private). ATLAS_BASE_URL always means a plain download.
use_gh=no
if [ -z "${ATLAS_BASE_URL:-}" ] && has gh && gh release list -R "$repo" --limit 1 >/dev/null 2>&1; then
  use_gh=yes
fi

version="${ATLAS_VERSION:-}"
if [ -z "$version" ]; then
  if [ "$use_gh" = yes ]; then
    version="$(gh release view -R "$repo" --json tagName --jq .tagName 2>/dev/null || true)"
  elif fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" 2>/dev/null; then
    version="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
  fi
  [ -n "$version" ] || fail "could not find the latest release (a private repository needs the GitHub CLI: gh auth login)"
fi
plain="${version#v}"
archive="${bin}_${plain}_${os}_${arch}.tar.gz"

say "Downloading $bin $version for $os/$arch..."
if [ "$use_gh" = yes ]; then
  gh release download "$version" -R "$repo" -D "$tmp" -p "$archive" -p checksums.txt ||
    fail "download failed: $archive from $repo $version"
else
  base="${ATLAS_BASE_URL:-https://github.com/$repo/releases/download/$version}"
  fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive (a private repository needs the GitHub CLI: gh auth login)"
  fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"
fi

# An exact name match: in a grep pattern the dots in the name match anything.
want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "$archive is not in checksums.txt"
got="$(sha "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (got $got, want $want)"

gzip -dc "$tmp/$archive" | (cd "$tmp" && tar -xf - "$bin") || fail "could not unpack $archive"
[ -f "$tmp/$bin" ] || fail "$archive holds no $bin"

# Copy next to the target, then rename over it: a running atlas-cli keeps its
# open file, and a failure leaves the old binary in place.
mkdir -p "$dir"
cp "$tmp/$bin" "$dir/.$bin.$$" || fail "cannot write to $dir"
chmod 755 "$dir/.$bin.$$"
mv -f "$dir/.$bin.$$" "$dir/$bin" || {
  rm -f "$dir/.$bin.$$"
  fail "cannot replace $dir/$bin"
}
installed="$("$dir/$bin" --version 2>/dev/null || say "$bin $version")"
say "Installed $installed to $dir/$bin"

# Releases up to v0.4.0 named the binary atlas; drop it so only one copy remains.
if [ -f "$dir/atlas" ] && "$dir/atlas" --version 2>/dev/null | grep -q '^atlas '; then
  rm -f "$dir/atlas" && say "Removed the old $dir/atlas (the command is now $bin)."
fi
case ":${PATH:-}:" in
  *":$dir:"*) ;;
  *) say "Add $dir to your PATH to run $bin." ;;
esac
