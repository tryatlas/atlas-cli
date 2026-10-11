# ADR-0013: Agents run in their own UIs; Atlas captures them through their hooks

**Status:** Accepted (2026-10-12, decided by the user: "forget about ACP; go
the same way the Entire CLI works, support the agents it supports, with the
user experience our app's way"). Supersedes ADR-0009 §1 and ADR-0012 as the
capture path. Amends SPEC §1.2 D4 and D20, and the ground rule on transcript
files (below).

## Context

ADR-0012 captured native-UI sessions by replaying them over ACP
(`session/list` + `session/load`). It worked for five agents, but a replay
carries no file writes, so native sessions had no file touches and no
checkpoints; it ran up to a minute late; and agents without `session/list`
(Gemini, Amp, Cline, …) were out.

The Entire CLI (`github.com/entireio/cli`, research in
`docs/research/entire-cli.md`) captures eight agents in their own UIs by
installing a hook into each agent's own config. The hook runs `entire hooks
<agent> <event>` with JSON on stdin at session start, each prompt, each turn
end and session end. At turn end it reads the agent's transcript from the
offset recorded at turn start and diffs `git status`, which yields prompts,
replies, tool calls, files edited, tokens and model, live. The Atlas desktop's
own checkpoint importer already reads Claude Code's transcript directory
(desktop ADR-0001, 2026-10-05 revision), so the principle has a precedent in
the product.

## Decision

1. **Capture is hook-driven.** `atlas agents add <agent>` writes an Atlas
   hook into the agent's config in the Project (`.claude/settings.local.json`,
   `.codex/hooks.json`, `.cursor/hooks.json`, `.github/hooks/atlas.json`,
   `.factory/settings.json`, `.agents/hooks.json`, `.opencode/plugins/atlas.ts`,
   `.pi/extensions/atlas/index.ts`). The agent runs the hidden command
   `atlas hooks <agent> <verb>` with its payload on stdin. One normalised
   event set (SessionStart, TurnStart, TurnEnd, Compaction, SessionEnd,
   SubagentStart, SubagentEnd, ToolUse) and one dispatcher; each agent only
   translates its payload and reads its own transcript format.
2. **The transcript rule is narrowed, not dropped.** A hook may read **only
   the transcript file the agent's own payload names** (or, for OpenCode, the
   agent's own `export` command), **from the offset recorded at turn start,
   only while the hook runs**. Atlas never scans an agent's directories for
   content, never reads another session's file, and never writes to an
   agent's storage. A backfill of past sessions is a separate, opt-in,
   later decision.
3. **Storage and identity stay Atlas's.** Turns go through the existing
   `Recorder` into `.atlas/sessions.db` (`source = acp`, the agent's id in
   `native_session_id`, so the desktop's cross-source skip prevents
   duplicates with its own Claude importer), then to the cloud through the
   existing sync. Session identity stays Atlas-minted (desktop ADR-0001).
   Commits are linked by the existing walk (`atlas capture link`), now also
   run from a `post-commit` git hook. No git refs, no commit trailers.
4. **Hooks never fail the agent.** Every hook exits 0; errors go to
   `.atlas/cli-hooks.log` and the last one shows in `atlas status`. Stdout
   carries only the agent's own response shape (the SessionStart banner).
   Every hook runs under a deadline. The installed command is wrapped in
   `sh -c 'if ! command -v atlas >/dev/null 2>&1; then exit 0; fi; exec atlas
   hooks …'` so an uninstalled `atlas` is silent.
5. **Delivery.** A hook writes to the store itself when it can take the
   writer lock; when `atlas serve` holds it, the hook posts the turn to the
   listener's owner-only Unix socket; when the desktop app holds it, the turn
   is spooled under `.atlas/hooks/spool/` and drained by the next writer.
6. **Shared memory reaches native UIs through MCP config.** `atlas agents
   add` also writes the `atlas_memory` server entry into the agent's MCP
   config as a stdio entry `atlas mcp-bridge --project <root> --agent <id>`
   (SPEC §7.3, no secret in any file). The bridge forwards to a running
   listener over the owner-only Unix socket, else serves memory in-process.
7. **Agents:** Claude Code, Codex, Cursor, GitHub Copilot CLI, Factory
   Droid, Antigravity, OpenCode, Pi; the per-agent facts come from the Entire
   research. Each hook file names only the `atlas` binary, never a path from
   the repository; hook commands are recognised by the prefix `atlas hooks `
   and stale Atlas entries are pruned on every install.
8. **ACP is parked**: `atlas run`, the chat screen, the registry installers
   and the ADR-0012 import loop stay in the tree under a hidden `atlas acp`
   group until their removal is agreed, and are no longer documented as the
   product.

## Consequences

- Native-UI sessions get file touches and checkpoints again, live.
- One integration per agent is the price; the external-agent protocol is
  a later addition.
- A hook-captured turn's file changes are attributed to the tool that wrote
  them when the transcript says so, else to the turn's last shell call.
- Tokens, model and subagent detail are as good as each agent's transcript.
- The hook files live in the Project; a teammate without `atlas` sees a
  silent no-op hook.
