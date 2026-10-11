# ADR-0009: ACP only, the app's way; the proxy path is parked

**Status:** Accepted (2026-10-11, decided by the user; amended the same day
from "primary + add-on" to "only"). Records SPEC §1.2 D20, which supersedes
D3, D4, D5, D7, D9 and D10 and reorders D8.

## Context

The CLI was first specified around the user's own agent command (D3): type
`claude`, and Atlas captures it through a model-traffic proxy and hands it
memory through the agent's MCP config file. That path needs one adapter per
agent (ADR-0000), captures Codex only in API-key mode (ADR-0004), cannot see
an agent whose provider URL is not configurable, and reconstructs sessions
from wire traffic instead of observing them.

The desktop does none of that. It is the ACP client: it launches every agent
from the registry, drives it over ACP, records the session from the delta
stream, hands memory in through `mcpServers` on `session/new`, and syncs the
record to the cloud Timeline. That works identically for every registry agent
and discovers capability on the wire (desktop ADR-0002). Phase 6 built the
proxy path first; Phase 7d had the ACP path as a secondary item.

The user asked for parity with the desktop: every ACP agent captured in
full, synced, and memory-aware, through the CLI.

## Decision

1. **`atlas run <agent>` is the whole product.** Nothing in the CLI is per agent: no config-file writers, no hooks, no proxy, no dialect parsers. It starts any registry agent
   (installing on first use, managed Node runtime, the desktop's launch rules
   and timeouts), drives it over ACP, and shows it in a Bubble Tea chat screen
   that does what the desktop chat panel does: streamed text and thinking,
   tool-call cards with status, permission prompts, elicitation forms (`form`
   and `url`), mode and model pickers from `modes`/`configOptions`, slash
   commands from `available_commands_update`, cancel with the 5 s grace, and
   the auth flow including terminal login commands.
2. **One recorder.** ACP sessions go through the same `internal/capture`
   recorder as the proxy path, with `source = 'acp'`, `native_session_id` =
   the ACP session id, `agent` = the registry id. Nothing is special-cased by
   agent id outside the launch-quirk table copied from the desktop.
3. **Memory over the wire.** The listener's HTTP `/mcp` endpoint with a
   per-session bearer minted in memory, offered as the `mcpServers` HTTP entry
   when the agent advertises `mcpCapabilities.http`, else the stdio bridge
   entry with `ATLAS_MCP_TOKEN` in the child's env (desktop ADR-0019). No
   config files are touched on this path.
4. **Cloud sync** runs for these sessions exactly as for proxy sessions
   (SPEC §6.7).
5. **Headless mode.** `atlas run <agent> --prompt "<text>" [--no-tui]`
   sends one prompt and exits after `end_turn`, for CI and cron.
6. **The proxy path is parked, not shipped.** `internal/proxy`,
   `internal/agents/ambient`, the `atlas shell`, `atlas env`, `atlas hook` and
   `atlas statusline` commands, and the hook/status-line/MCP-config writers
   from Phase 6 move to a `parked/proxy-path` branch and are removed from
   `main`. The `internal/capture` store and recorder, `internal/listener`
   (socket + `/mcp`), the redactor and everything else stay. ADR-0000,
   ADR-0003 and ADR-0004 are marked Superseded by this ADR; ADR-0002's
   `proxy` source value stays reserved on the desktop but the CLI has nothing
   that writes it.

## Consequences

- The chat TUI is now the largest single piece of work in the repo. The
  desktop's chat behaviour is the reference (`evidence-acp.md` §3 lists every
  method, notification and client callback it handles).
- A user on the ACP path loses whatever lives only in an agent's native UI.
  The slash picker is sourced purely from ACP advertisement (desktop
  ADR-0005), so commands an agent does not advertise are not offered.
- Agent login works through ACP `authMethods`: a typed `terminal` method runs
  the agent's own binary with the given args, a `_meta["terminal-auth"]`
  spec runs that command; the CLI never guesses a login command.
- ADR-0000, ADR-0003 and ADR-0004 are Superseded. ADR-0002's `proxy` source
  stays reserved on the desktop; the CLI writes only `acp`.
- Memory reaches agents only over `mcpServers` (HTTP with bearer, or the
  desktop-style stdio bridge with `ATLAS_MCP_TOKEN` in the child env), so the
  Unix-socket peer-credential design of ADR-0003 is no longer needed; the
  listener keeps the loopback HTTP `/mcp` endpoint the desktop uses.
- The Phase 6 live verification of the capture store and recorder against a
  desktop-written database still stands; that code is reused unchanged.
