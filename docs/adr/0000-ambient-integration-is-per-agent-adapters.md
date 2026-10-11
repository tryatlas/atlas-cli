# ADR-0000: Ambient integration is per-agent adapters behind one interface

**Status:** Accepted (2026-10-10). Records locked decision D7 (SPEC §1.2). **Superseded 2026-10-11 by ADR-0009 (ACP only): the proxy/ambient path is parked and not shipped.**

Numbering: ADRs in this repo are numbered on their own. Desktop ADRs are cited
as "desktop ADR-00NN" and live in `docs/atlas-reference/adr/`. ADR-0001 to
ADR-0007 here record SPEC §1.2 D13 to D19 (the former §10 open decisions O1 to
O7), in that order.

## Context

Users run their normal agent command (`claude`, `codex`, `gemini`), not an
Atlas wrapper (D3). Giving those agents shared memory and capturing their
sessions means touching each agent's own surfaces:

- its MCP config file, to offer the memory server (D5);
- the env var or config key that sets its model provider base URL, to route
  traffic through the capture proxy (D4);
- its hooks, for session identity;
- its status line, if it has one (D10);
- the wire dialect of its provider (Anthropic Messages, OpenAI Responses,
  Gemini).

Each agent does these differently, and there is no protocol to discover them
the way ACP advertises capabilities. Some code has to know which agent it is
dealing with.

Desktop ADR-0002 says no agent gets special treatment: behaviour is gated on
advertised capabilities, never on an agent id. That rule was written for the
ACP path, where capabilities *are* advertised. The ambient path has nothing
to gate on.

## Decision

**On the ambient path, integration is per agent, and all of it sits behind one
interface.** Each supported agent has exactly one adapter in
`internal/agents/ambient/<id>/`, implementing the `Adapter` interface from
SPEC §7.1. Nothing outside `internal/agents/ambient/` may branch on an agent
id.

Rules every adapter follows:

1. **Writes are idempotent and marked.** An entry the CLI writes carries a
   well-known key (`atlas` server name, `atlas hook …` command) or a comment,
   so a second install changes nothing.
2. **Writes are recorded.** Every change is listed in `.atlas/cli.json`, and
   removal undoes exactly what is listed there and nothing else.
3. **User entries are merged, never replaced.** A JSON or TOML file is read,
   the CLI's key is set, and the rest goes back unchanged (order kept, TOML
   comments kept where the library allows).
4. **Project scope first.** An adapter writes to a user-level file only when
   the agent offers no project-level route for that setting. It records why
   in a code comment, and `atlas status` names the user-level change.
5. **No agent storage is read** (desktop ADR-0001): no transcripts, no state
   DBs, no auth files. Identity comes from hooks and the wire.
6. **Nothing is injected into the model's context** (D6, desktop ADR-0010):
   hooks may print a user-visible `systemMessage`, never
   `additionalContext`.
7. **An adapter is code, not data.** Adding an agent means adding an adapter.
   There is no plugin loading.

**The ACP path keeps desktop ADR-0002.** `atlas run` (§8) uses the registry
and advertised capabilities. The only id-keyed data there is the launch-quirk
table (SPEC §8.3), kept as data, as on the desktop. The ACP client must not
import `internal/agents/ambient`.

## Consequences

- Supporting another agent is a reviewable unit: one directory, one
  interface, plus a parser for its wire dialect if it is a new one.
- Agents the CLI does not know get neither memory nor capture outside
  `atlas run`. That is accepted: there is no agent-agnostic way to do it.
- The interface may need to grow. SPEC §7.1's `ProxyEnv` cannot express
  Codex's routing, which is a config file and not an env var (see ADR-0004).
  An adapter needs a hook that runs when the shell starts and can add
  env vars *and* PATH shims. Changing the interface is an amendment to this
  ADR.
- Adapters are where external-agent facts live, and those facts drift
  (config shapes, hook payloads). Each adapter keeps a test fixture of the
  file shape it writes, and the date that shape was checked against the
  agent's docs.
