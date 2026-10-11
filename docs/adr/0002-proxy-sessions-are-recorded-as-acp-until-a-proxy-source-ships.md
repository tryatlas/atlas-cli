# ADR-0002: Proxy-captured sessions are recorded with `source = 'acp'` until a `proxy` source ships (O2)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D14 (formerly §10 O2).

## Context

`agent_session.source` takes `acp`, `native` or `external_jsonl` on the
desktop, which parses it strictly (`model.rs:26-33`, evidence-capture.md:351).
A session captured by the CLI's proxy is none of these. What each value does:

| value | effect on the desktop |
|---|---|
| `acp` | live session; eligible for Checkpoints (evidence-capture.md:27); evidence-capture.md:708 already tells a CLI to use it |
| `native` | means the Atlas engine; plainly wrong |
| `external_jsonl` | "imported from the agent's transcript": not live, no Checkpoints, and it shares a unique key with the desktop's own importer rows |
| a new `proxy` | the true value; an unknown value fails the strict parse on any desktop that does not know it, which breaks D11 |

There is a second problem behind the first. `atlas init` writes
`import_approved = 1` (SPEC §4.4). When the desktop opens the folder, its
importer reads `~/.claude/projects/<cwd>/*.jsonl` and records each Claude Code
session as `external_jsonl`. It skips a session that already exists under
another source, matching on `native_session_id` (`import.rs:26-31,357-369`,
evidence-capture.md:51). That skip only works if the CLI stores **Claude
Code's own session id** as `native_session_id`.

## Decision

1. **Write `source = 'acp'` for proxy-captured sessions in v1.** The desktop
   opens the store, shows the session as live, and links its commits.
2. **`native_session_id` is the agent's own session id whenever one can be
   had**: the `session_id` from Claude Code or Gemini CLI hooks, Codex's
   `thread_id`. The SPEC §7.2 fallbacks (`metadata.user_id` uuid, a hash of
   the first message) are used only when no hook has fired. A
   fallback-identified Claude session may be imported a second time by the
   desktop; `atlas status` counts those sessions.
3. **Ask the desktop and ingest teams to add `proxy`** to the enum, read
   leniently. The CLI switches when both have shipped, behind a `cli.toml`
   setting that defaults off until then. The switch gets an amendment to this
   ADR.

   *Amendment 2026-10-10:* the desktop side is done. `Source::Proxy` was added
   to `crates/atlas-checkpoint/src/model.rs` (`as_str`, `parse`, and
   `is_live` includes it), shared memory's session lookup tries it, and a
   test covers the round trip; it sits uncommitted on the desktop branch
   `feat/commit-tab-history` and ships with the next desktop release. The
   ingest server still has to accept the value. Until both are released the
   CLI keeps writing `acp`.

## Consequences

- The desktop and the ingest server cannot tell a proxy session from an
  ACP-hosted one until (3) lands. Nothing on either side branches on that
  difference today, apart from liveness and Checkpoints, which we want.
- Without hooks installed, Claude Code sessions captured by the proxy can be
  duplicated by the desktop importer. So `atlas capture enable` installs the
  Claude Code hooks by default (SPEC §7.4); without them capture still works,
  with a warning.
- *Checked 2026-10-10:* the importer's cross-source skip is keyed on
  **workspace and native id**: `store.native_session_exists(workspace_id, native_session_id)`
  (`crates/atlas-checkpoint/src/import.rs:357-369`), so a CLI session is
  only protected from re-import when its `workspace_id` is the same canonical
  path the desktop would use for that folder (SPEC §4.1).
