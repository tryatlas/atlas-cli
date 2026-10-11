# ADR-0006: Windows is out of scope for v1 (O6)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D18 (formerly §10 O6).

## Context

The CLI targets servers. Several pieces it depends on are Unix-only or
unverified on Windows: process-group kill (`setsid`/`killpg`, desktop
ADR-0008), Unix-socket peer credentials (ADR-0003), `$SHELL -i` for
`atlas shell`, and the stdio bridge as a subprocess (desktop ADR-0019 calls
for a manual check). The desktop's Windows support is itself partial
(evidence-acp.md:169-172).

## Decision

- v1 builds and tests linux/amd64, linux/arm64 and darwin/arm64 only. The
  release and CI matrix have no Windows target. darwin/amd64 is not in CI
  either but should build; it is added when someone asks.
- OS-specific code sits in `_unix.go` / `_linux.go` / `_darwin.go` files
  behind a small function, so a Windows port adds files rather than editing
  shared ones.
- Path handling uses `path/filepath` for the OS and `/` only where a stored
  format requires it (repo-relative paths, SPEC §6.3).

## Consequences

- `go build` for windows is not promised to work, and CI does not check it.
- A future Windows ADR has to decide: the session file under `%APPDATA%`, the
  bridge's authentication (named pipe with an ACL instead of peer
  credentials), a shell for `atlas shell`, and Job Objects in place of
  process groups.
