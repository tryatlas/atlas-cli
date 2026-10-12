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
#   ATLAS_THEME        light or dark (default: from COLORFGBG, else dark)
#   NO_COLOR           plain output, as when not on a terminal
set -eu

repo="tryatlas/cli"
bin="atlas-cli"
title="Installing $bin"

# --- The look -------------------------------------------------------------
# The CLI's design system (internal/tui/theme.go): the Atlas palette,
# monochrome first, colour only for status; gold tree lines, a green check,
# a red cross, the cream logo. On a terminal the steps draw as the CLI's
# work view; elsewhere they are the plain "Label:  value" lines the CLI
# prints without a terminal.

fancy=no
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ]; then
  fancy=yes
fi
fancy_err=no
if [ "$fancy" = yes ] && [ -t 2 ]; then
  fancy_err=yes
fi

case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
  *[Uu][Tt][Ff]-8* | *[Uu][Tt][Ff]8*) g_tree="├" g_ok="✓" g_err="✗" g_dot="●" ;;
  *) g_tree="|" g_ok="+" g_err="x" g_dot="*" ;;
esac

light=no
case "${ATLAS_THEME:-}" in
  [Ll][Ii][Gg][Hh][Tt]) light=yes ;;
  [Dd][Aa][Rr][Kk]) ;;
  *) case "${COLORFGBG:-}" in *\;7 | *\;15) light=yes ;; esac ;;
esac

esc="$(printf '\033')"
s_reset="" s_bold="" s_faint="" s_label="" s_tree="" s_ok="" s_err="" s_warn="" s_logo=""
if [ "$fancy" = yes ]; then
  s_reset="$esc[m"
  case "${COLORTERM:-}${TERM:-}" in
    *256* | *truecolor* | *24bit*)
      # The palette as the CLI draws it on a 256-colour terminal.
      if [ "$light" = yes ]; then
        s_bold="$esc[1;38;5;16m" s_faint="$esc[38;5;243m" s_label="$esc[1;38;5;245m"
        s_tree="$esc[38;5;136m" s_ok="$esc[38;5;65m" s_err="$esc[1;38;5;160m"
        s_warn="$esc[38;5;136m" s_logo="$esc[1;38;5;16m"
      else
        s_bold="$esc[1;38;5;231m" s_faint="$esc[38;5;246m" s_label="$esc[1;38;5;241m"
        s_tree="$esc[38;5;180m" s_ok="$esc[38;5;150m" s_err="$esc[1;38;5;203m"
        s_warn="$esc[38;5;226m" s_logo="$esc[1;38;5;230m"
      fi
      ;;
    *)
      s_bold="$esc[1m" s_faint="$esc[2m" s_label="$esc[1;2m" s_tree="$esc[33m"
      s_ok="$esc[32m" s_err="$esc[1;31m" s_warn="$esc[33m" s_logo="$esc[1m"
      ;;
  esac
fi

# paint STYLE TEXT: TEXT in STYLE, reset after.
paint() { printf '%s%s%s' "$1" "$2" "$s_reset"; }

# banner: the splash's logo, the three-stripe "///" mark in dots, with the
# name beside it.
banner() {
  [ "$fancy" = yes ] || return 0
  case "$g_dot" in "●") ;; *) return 0 ;; esac
  dots="$(paint "$s_logo" "●●●●")"
  printf '\n'
  # Each row: the stripes one column further left than the row above, and
  # padded to the logo's 24 columns. (POSIX printf has no * widths.)
  pad="      " tail=""
  for side in "" "" \
    "$(paint "$s_bold" Atlas) $(paint "$s_faint" installer)" \
    "$(paint "$s_faint" "Shared memory and session capture")" \
    "$(paint "$s_faint" "for coding agents.")" "" ""; do
    printf '  %s%s   %s   %s%s    %s\n' "$pad" "$dots" "$dots" "$dots" "$tail" "$side"
    pad="${pad# }" tail="$tail "
  done
  printf '\n'
}

# step LABEL VALUE: one finished step of the work view.
step() {
  if [ "$fancy" = yes ]; then
    printf '%s %s\n' "$(paint "$s_tree" "$g_tree")" "$(paint "$s_faint" "$1  $2")"
  else
    printf '%-12s %s\n' "$1:" "$2"
  fi
}

# done_ MSG: the work view's closing line.
done_() {
  if [ "$fancy" = yes ]; then
    printf '%s %s\n' "$(paint "$s_ok" "$g_ok")" "$(paint "$s_bold" "$1")"
  else
    printf '%s\n' "$1"
  fi
}

# warn MSG: a status dot in the warning colour.
warn() {
  if [ "$fancy" = yes ]; then
    printf '%s %s\n' "$(paint "$s_warn" "$g_dot")" "$1"
  else
    printf '%s\n' "$1"
  fi
}

# next: what to run now, set as the splash's command groups.
next() {
  if [ "$fancy" = yes ]; then
    printf '\n  %s\n' "$(paint "$s_label" START)"
    printf '  %s  %s\n' "$(paint "$s_bold" "$bin login ")" "$(paint "$s_faint" "Sign in with your Atlas account")"
    printf '  %s  %s\n' "$(paint "$s_bold" "$bin init  ")" "$(paint "$s_faint" "Set up shared memory and capture in a project folder")"
    printf '  %s  %s\n\n' "$(paint "$s_bold" "$bin       ")" "$(paint "$s_faint" "Every command")"
  else
    printf 'Next: %s login, then %s init in a project folder.\n' "$bin" "$bin"
  fi
}

# fail MSG: the work view's error line, then exit.
fail() {
  if [ "$fancy_err" = yes ]; then
    printf '%s %s %s\n' "$(paint "$s_err" "$g_err")" "$(paint "$s_bold" "$title")" "$(paint "$s_err" "$*")" >&2
  else
    printf '%s install: %s\n' "$bin" "$*" >&2
  fi
  exit 1
}

# --- The install ------------------------------------------------------------

has() { command -v "$1" >/dev/null 2>&1; }

dir="${ATLAS_INSTALL_DIR:-${HOME:-}/.local/bin}"
[ -n "${ATLAS_INSTALL_DIR:-}" ] || [ -n "${HOME:-}" ] || fail "HOME is not set; set ATLAS_INSTALL_DIR"

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
  fetch() { curl -fsL "$1" -o "$2"; }
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

banner
step Platform "$os/$arch"

# use_gh: the GitHub CLI is signed in and can see the repository (needed
# while it is private). ATLAS_BASE_URL always means a plain download.
use_gh=no
if [ -z "${ATLAS_BASE_URL:-}" ] && has gh && gh release list -R "$repo" --limit 1 >/dev/null 2>&1; then
  use_gh=yes
fi

version="${ATLAS_VERSION:-}"
if [ -z "$version" ]; then
  step Checking "the latest release on GitHub"
  if [ "$use_gh" = yes ]; then
    version="$(gh release view -R "$repo" --json tagName --jq .tagName 2>/dev/null || true)"
  elif fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" 2>/dev/null; then
    version="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
  fi
  [ -n "$version" ] || fail "could not reach GitHub for the latest release (set ATLAS_VERSION to pick one)"
fi
step Release "$version"
plain="${version#v}"
archive="${bin}_${plain}_${os}_${arch}.tar.gz"

step Downloading "$archive"
if [ "$use_gh" = yes ]; then
  gh release download "$version" -R "$repo" -D "$tmp" -p "$archive" -p checksums.txt ||
    fail "download failed: $archive from $repo $version"
else
  base="${ATLAS_BASE_URL:-https://github.com/$repo/releases/download/$version}"
  fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive (is $version a release with a $os/$arch build?)"
  fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"
fi

# An exact name match: in a grep pattern the dots in the name match anything.
want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "$archive is not in checksums.txt"
got="$(sha "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (got $got, want $want)"
step Verified "$archive (sha256)"

gzip -dc "$tmp/$archive" | (cd "$tmp" && tar -xf - "$bin") 2>/dev/null || fail "could not unpack $archive"
[ -f "$tmp/$bin" ] || fail "$archive holds no $bin"

# Copy next to the target, then rename over it: a running atlas-cli keeps its
# open file, and a failure leaves the old binary in place.
mkdir -p "$dir" 2>/dev/null || fail "cannot create $dir"
cp "$tmp/$bin" "$dir/.$bin.$$" 2>/dev/null || fail "cannot write to $dir"
chmod 755 "$dir/.$bin.$$"
mv -f "$dir/.$bin.$$" "$dir/$bin" || {
  rm -f "$dir/.$bin.$$"
  fail "cannot replace $dir/$bin"
}
step Placed "$dir/$bin"

# Releases up to v0.4.0 named the binary atlas; drop it so only one copy remains.
if [ -f "$dir/atlas" ] && "$dir/atlas" --version 2>/dev/null | grep -q '^atlas '; then
  rm -f "$dir/atlas" && step Removed "the old $dir/atlas (the command is now $bin)"
fi

installed="$("$dir/$bin" --version 2>/dev/null || printf '%s %s' "$bin" "$plain")"
done_ "Installed ${installed}."
case ":${PATH:-}:" in
  *":$dir:"*) ;;
  *) warn "$dir is not on your PATH; add it to run $bin." ;;
esac
next
