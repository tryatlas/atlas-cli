# ADR-0011: v1 also ships for Intel Macs (darwin/amd64)

**Status:** Accepted (2026-10-11, at the user's request: "release mac and
linux first"). Amends SPEC §1.2 D18, which listed linux/amd64, linux/arm64
and darwin/arm64.

## Context

D18 left Intel Macs out of the v1 targets. The machine the CLI was built
and live-tested on is an Intel Mac (darwin-x86_64): every live agent run,
the chat-screen session and `atlas agents check --all` ran there. Leaving it
out would ship a release that cannot be installed on the one platform where
it was verified.

The cost is small. The CLI is pure Go, so darwin/amd64 cross-compiles like
the other targets. GitHub provides a native Intel runner (`macos-15-intel`)
so the tests run on it, not just a compile. The ACP registry and the managed
Node runtime both have darwin-x86_64 builds (`darwin-x86_64`, `darwin-x64`).

## Decision

Release for **linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64**.
CI tests all four on native runners; GoReleaser builds four archives;
`install.sh` installs on all four. Windows stays out (ADR-0006).

## Consequences

- Each release has one more archive and a fourth CI job.
- Registry agents without a darwin-x86_64 build (three today) cannot run on
  Intel Macs; `atlas agents list` says so per agent.
