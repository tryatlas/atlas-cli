# Atlas CLI — the complete hand-off folder

Everything a session needs to build the headless Atlas CLI (Go + Bubble Tea,
its own repository) without access to the Atlas desktop source. Copy this
whole folder into the new repo as `docs/atlas-reference/` and start with
`SPEC.md`.

| file | what it is | read when |
|---|---|---|
| `SPEC.md` | The specification: product journey, locked decisions, architecture, and every wire and file format the CLI must match (auth, `.atlas/`, shared memory, session capture, ambient agent integration, ACP path, Go stack, open decisions). | **First. Always.** |
| `evidence-auth.md` | Desktop auth subsystem, every claim cited to `file:line` in the atlas repo: endpoints with real JSON, device grant, session file, token rules, how other hosts receive the token. | When implementing `internal/auth` or when SPEC §3 is not enough. |
| `evidence-memory.md` | Shared memory: vocabulary, SQLite v6 schema, write/guard/fold rules, hash chain, MCP server and tools verbatim, redaction, what has no backend sync. | When implementing `internal/memory` or when SPEC §5 is not enough. |
| `evidence-capture.md` | Session capture: SQLite v10 schema, the delta wire, the recording pipeline per event, git linkage, redaction, ingest sync protocol, full `.atlas/` folder map. | When implementing `internal/capture`, `internal/proxy` mapping, or SPEC §6. |
| `evidence-acp.md` | ACP agents: registry, install, managed Node, launch, handshake, every method and notification the desktop uses, MCP injection, timeouts, per-agent quirks. | When implementing `atlas run` or SPEC §8. |
| `CONTEXT.md` | The desktop's glossary. The source of truth for what a word means (Thread, Session, Project, Workspace, Organisation, Member, Owner, the seven memory kinds, working vs durable memory, revision, candidate, stale…). | Whenever a term is ambiguous. |
| `adr/0001-…` | Session identity is Atlas-minted; Atlas never scrapes another program's storage. | Before any idea of reading `~/.claude` or `~/.codex`. |
| `adr/0002-…` | No default ACP agents; no agent gets special treatment on the ACP path. | Before adding an agent list or an id check on the `atlas run` path. |
| `adr/0005-…` | `~/.agents/skills` is the canonical skill store; the slash picker comes purely from ACP advertisement; nothing is injected into a session besides MCP servers. | Before injecting anything else into `session/new`. |
| `adr/0008-…` | Every hop of an agent start path is bounded by a timeout (values). | When wiring `atlas run`. |
| `adr/0010-…` | Shared memory is pulled through the tool server, never pushed into context. | Before any temptation to inject memory via the proxy. |
| `adr/0017-…` | Memory revisions are canonical; the entry is a view; the chain is hashed. | Before touching `revisions` or the chain encoding. |
| `adr/0018-…` | Memory is used only as far as its evidence holds (citations, commit evidence, stale). | When implementing citation checks in the MCP tools. |
| `adr/0019-…` | A stdio bridge carries memory to agents without HTTP MCP. | When implementing `atlas mcp-bridge`. |
| `redact-README.md` | The redactor's contract: six layers, runs before persistence, what it deliberately leaves alone. | When porting `internal/redact`. |

`CONTEXT.md` and the evidence files also mention ADR-0003, 0011, 0012 and 0014 (the native agent, its rename, the UI and organisation tool servers). Those are desktop-only features and are deliberately not copied here.

Provenance: read from `github.com/pacifio/atlas` at branch
`feat/commit-tab-history` (commit `062699ab`) on 2026-10-10. External-agent
facts (Claude Code, Codex, Gemini CLI, Bubble Tea v2) were checked against
their public docs the same day; SPEC marks the few that could not be confirmed.

Verification: on 2026-10-10 every `file:line` citation in the four evidence
files and every derived statement in SPEC §3, §5, §6 and §8 was re-opened
against the working tree (984 claims). The 53 findings — mostly line drift,
plus a handful of precision fixes (title redaction path, Latin-1 composition
table, retry-only-for-active-entries, missing-file citation validity, archive
suffix list, `Retry-After` semantics, slug fallback for `workspaceId`) — were
folded back in. Uncommitted `#[cfg(test)]` lines in three memory files shift
a few cited line numbers by one; the evidence files say where.

Decisions already made are SPEC §1.2 (D1–D19); do not re-open them. D13–D19
were the former open questions O1–O7, resolved 2026-10-10; SPEC §10 maps the
old numbers to the new. Each of D13–D19 still gets a one-page ADR in the new
repo recording the alternatives considered.
