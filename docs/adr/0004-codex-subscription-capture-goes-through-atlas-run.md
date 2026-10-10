# ADR-0004: Codex under a ChatGPT subscription is captured only through `atlas run codex` (O4)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D16 (formerly §10 O4).

## Context

Codex under a ChatGPT subscription talks to OpenAI over its own transport and
ignores any OpenAI-compatible base URL (SPEC §7.2). The proxy cannot see that
traffic. The options:

1. **`notify` hook.** It fires at turn end with a JSON payload (`thread_id`,
   the input messages, the last assistant message). There are no tool calls,
   no file touches and no usage. It has to sit at the top of the user-level
   `~/.codex/config.toml`, and Codex allows one `notify` command, so the CLI
   would replace a user's existing notifier or have to chain it.
2. **`atlas run codex`** over ACP (codex-acp, which drives `codex app-server`):
   full fidelity through the same recorder as the desktop (SPEC §8.4), and it
   works with subscription auth.
3. **Nothing ambient** for subscription users.

A related problem affects every Codex user, not only subscription users. The
SPEC routes API-key Codex through the proxy with a **user-level**
`model_provider = "atlas"`, because project config ignores it. That setting
also applies outside `atlas shell`. Once the shell exits, the listener is gone
and every `codex` run fails to connect. For a ChatGPT-login user it would also
switch auth to `OPENAI_API_KEY`.

## Decision

- **Subscription Codex:** memory works (MCP does not depend on auth mode).
  Capture is only through `atlas run codex`. The ambient path records nothing,
  and `atlas status` says so in one line, naming `atlas run codex` as the
  alternative. No `notify` hook in v1.
- **API-key Codex:** the provider **definition**
  (`[model_providers.atlas]`) may go in `~/.codex/config.toml`; it does
  nothing until selected. It is never **selected** there. Inside
  `atlas shell`, a `codex` shim on a shell-only PATH entry runs the real
  binary with `-c model_provider=atlas` prepended. Outside the shell Codex
  behaves exactly as before.
- **Which mode is in use** is found by running `codex login status`, never by
  reading `~/.codex/auth.json` (desktop ADR-0001; it holds credentials). The
  shim selects the Atlas provider only in API-key mode.

## Consequences

- The adapter needs a shell-start step that adds PATH shims, which SPEC
  §7.1's `ProxyEnv` cannot express (amends ADR-0000's interface).
- A user who runs Codex with the full path to its binary inside the shell
  bypasses the shim and is not captured. Accepted.
- **Unverified:** the output of `codex login status` and its stability across
  versions; whether `-c model_provider=…` on the command line still overrides
  config in current Codex. Both are checked against current Codex before the
  Codex adapter is written (Phase 7).
- A thin capture from `notify` can be added later as its own ADR if users ask
  for it.
