# ADR-0012: Agents run in their own UIs; Atlas captures their sessions over ACP

**Status:** Accepted (2026-10-11, decided by the user: "I don't want to give
our TUI for agents; I want all agents to run in their native TUI"). Amends
ADR-0009 §1: the Atlas chat screen is no longer the only way, and not the
recommended one.

## Context

ADR-0009 made `atlas run <agent>` and its chat screen the product. The user
wants people to keep each agent's own interface (Claude Code's TUI,
Codex's TUI, OpenCode, Gemini CLI…) and still get Atlas capture and sync.

The desktop app already does this for every agent. Its stores hold
`source = acp` sessions from opencode, codex-acp, cursor and gemini, and
the user saw sessions started in a plain terminal appear in the app. Only
Claude Code's terminal sessions come in through the file importer
(`external_jsonl`). evidence-acp.md mentions a per-agent history backfill
(`BACKFILL_TIMEOUT`, `SYNC_AGENT_TIMEOUT`/`SYNC_DEBOUNCE`, lines 244-245)
but not that it writes capture rows. The stores show that it does.

A live probe with the CLI settled the key question. Sessions started in an
agent's native CLI are listed by the same agent's ACP side for the same
folder (`session/list`), and can be replayed (`session/load`). This was
checked for OpenCode (`opencode run`), Claude Code (`claude -p`) and Codex
(`codex exec`).

## Decision

1. **Agents run in their own UIs.** `atlas capture import` asks every
   installed agent that advertises `sessionCapabilities.list` and
   `loadSession` for this Project's sessions and replays each new or
   changed one. Each replay is recorded as the desktop records them:
   `source = acp`, `native_session_id` = the agent's session id,
   `agent` = the registry id, every text through the redactor.
2. **Re-imports only append.** A session already holding N turns gets the
   replay's turns after N. `.atlas/cli-import.json` (the CLI's own file)
   remembers when each session was last imported. A session is replayed
   again only when its listed `updatedAt` is later.
3. **`atlas serve` imports in the background** (`--import-every`, default
   1 minute). It keeps one connection per agent alive between passes and
   reconnects one that ended. `atlas serve --daemon` once per Project, and
   sessions started in any native UI are captured and synced on their own.
4. **No per-agent code, no transcript files.** Discovery and content come
   only over ACP; the ground rule "never read another agent's transcript
   files" stands. An agent that cannot list or load sessions is skipped
   with that reason.
5. `atlas run` and the chat screen stay, for headless runs (CI, cron) and
   for anyone who wants them.

## Consequences

- A replay carries no file writes, so imported turns have **no file
  touches and no checkpoints**, like the desktop's imports. Live
  `atlas run` sessions keep both.
- A replay carries no timestamps. Imported rows are stamped at import time,
  in replay order.
- Usage is whatever the replay carries, usually none.
- Agents that do not advertise both list and load (Gemini today) are not
  imported.
- **Shared memory** reaches a native UI only through that agent's own MCP
  configuration. That needs one config writer per agent (the parked D5
  work) and is a separate decision.
- When the desktop app has the folder open it holds the writer lock and
  does its own backfill. `atlas capture import` then says so instead of
  writing.
