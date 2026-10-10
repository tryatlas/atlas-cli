# ADR-0003: No memory secret is written to agent config files; the bridge authenticates by peer credentials (O3)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D15 (formerly §10 O3). **Superseded 2026-10-11 by ADR-0009 (ACP only): the proxy/ambient path is parked and not shipped.**

## Context

SPEC §7.3 has ambient agents reach memory through a stdio bridge entry in
their project config (`atlas mcp-bridge --project <dir>`), carrying a
per-project bearer token in `ATLAS_MCP_TOKEN`. The token ends up in one of
these places:

- `.mcp.json`, `.gemini/settings.json`, `.codex/config.toml`: project files
  users commit;
- `${ATLAS_MCP_TOKEN}` expansion: works for Claude Code only, and the bridge's
  own-start path then reads the token from `~/.config/atlas/cli.toml`.

Either way a secret persists on disk. That conflicts with D12 and with this
repo's ground rule that the session token file (mode 0600) is the only secret
that persists.

On the desktop the token does two jobs (evidence-memory.md:208): it
**authenticates** the caller, and it carries the **grant**
`{session_id, agent, cwd}` that names who is writing. Tokens are minted per
session, held in memory and revoked at session end. Ambient agents have no
session-start handshake with Atlas, so neither job can be done the desktop way.

## Decision

**Authentication: peer credentials over a Unix socket, not a bearer token.**

- The listener started by `atlas shell` / `atlas serve` also listens on a Unix
  domain socket at `$XDG_RUNTIME_DIR/atlas/<16 hex of sha256(launch dir)>.sock`,
  falling back to `$TMPDIR/atlas-<uid>/…`. The directory is mode 0700 and the
  socket 0600. The path is published in `.atlas/cli-run.json`.
- On accept, the listener checks that the peer's uid equals its own
  (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on darwin, through
  `golang.org/x/sys/unix`, without cgo). Anything else is closed unanswered.
- The bridge entry written to agent configs holds no secret:
  `{"command":"atlas","args":["mcp-bridge","--project","<launch dir>","--agent","<id>"]}`.
- With no listener running, the bridge serves memory in-process (SPEC §7.3),
  so no authentication is needed.

**Grant: from the bridge, not a token.** The bridge sends `agent` (from
`--agent`) and `cwd` (its working directory, else `--project`). It sends a
`session_id` minted once per bridge process. Claude Code and Gemini spawn one
stdio server per agent session, so one bridge process lives as long as one
session. Where the listener can match the bridge to a hook-registered session
(the same parent process), it uses the hook's `session_id` instead, so memory
and capture name the same session.

**The HTTP `/mcp` endpoint and bearer tokens stay**, exactly as the desktop
does them, for the ACP path (`atlas run`, SPEC §8.4) and for the desktop-style
`mcp-bridge <url>` form (desktop ADR-0019). Those tokens are minted per
session and held in memory only.

## Consequences

- Nothing secret lands in a file a user might commit, and `cli.toml` holds no
  token.
- Any process of the same user can reach memory through the socket. That is
  the same trust boundary as the files themselves: such a process can already
  open `memory.sqlite`.
- Windows has no equivalent here (see ADR-0006).
- **Needs a live check:** that Claude Code, Gemini CLI and Codex each spawn
  one stdio MCP process per session and not one per app. If one of them
  shares a process, its sessions share one memory session id. That costs the
  "same session" leg of the `expected_revision` guard for that agent; it does
  not break correctness.
- **Needs a live check:** matching parents between hook and bridge processes
  (hooks run through a shell). Until that is proven, memory sessions and
  capture sessions for ambient agents may carry different ids, and
  `memory_why` names fewer sessions.
