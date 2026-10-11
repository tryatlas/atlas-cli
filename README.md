# atlas-cli

A headless Atlas for servers: a Go binary that signs in like the Atlas desktop
app, binds a folder to an organisation and a cloud Project, and gives every
coding agent run in that folder **session capture** and **shared memory**,
byte-compatible with the desktop's on-disk formats. The agents keep their own
UIs: Atlas plugs into their hooks (ADR-0013).

## Install

Linux (amd64, arm64) and macOS (Apple silicon, Intel):

```sh
curl -fsSL https://github.com/tryatlas/atlas-cli/releases/latest/download/install.sh | sh
```

It verifies the archive's sha256 and installs `atlas` to `~/.local/bin`
(`ATLAS_INSTALL_DIR` to change it, `ATLAS_VERSION=v1.2.3` to pin). From
source: `make build` (pure Go, no cgo).

## Use

```
atlas login                     # device grant against the Atlas auth server
cd ~/work/backend
atlas init                      # org + cloud Project, writes .atlas/ (--local: no cloud)
atlas agents add claude-code    # the agents you use here (atlas agents list shows all eight)
atlas agents add codex cursor
claude                          # …or codex, cursor-agent, opencode: work as usual
atlas status                    # sessions captured, commits linked, sync
```

`atlas agents add` writes two things into the agent's own config in the
Project: its **hook**, which runs `atlas hooks <agent> <event>` at session
start, each prompt, each turn end and session end; and the **`atlas_memory`
MCP server**, so the agent can read and write shared memory. It also installs a
git `post-commit` hook that links sessions to the commits that carry their
work. Nothing is wrapped and no environment variable changes; remove it all with
`atlas agents remove <agent>`.

Agents run their project hooks only for a project you trust: Codex, Copilot
and Pi each ask once in their own UI. Keep `atlas` current on PATH, including
`~/.local/bin`, which some agents put first.

Capture is per event. The prompt opens the turn the moment you submit it;
each tool call lands as the agent's post-tool hook fires, with the file it
wrote; at turn end the hook reads the agent's transcript from where the turn
started, diffs `git status`, and closes the turn with the reply, token usage
and the model. Every event goes to the cloud Project right away: the hook
writes the store itself and syncs, or hands the event to a running
`atlas serve` (which syncs it at once), or spools it when the desktop app has
the folder open. Someone watching the Project sees the prompt, then the tool
cards one by one, then the reply. Text is not streamed token by token: no
agent exposes a hook for that. A hook never fails the agent: errors go to
`.atlas/cli-hooks.log` and `atlas status`.

| agent | hook written to | memory server written to |
|---|---|---|
| `claude-code` Claude Code | `.claude/settings.local.json` | `.mcp.json` (+ approved in the local settings) |
| `codex` Codex | `.codex/hooks.json`: the first interactive `codex` here asks "Hooks need review"; choose **Trust all and continue** (`codex exec` cannot ask) | `.codex/config.toml` (trusted projects) |
| `cursor` Cursor CLI and IDE | `.cursor/hooks.json` (`cursor-agent -p` fires no turn hooks; such a session is captured at its end) | `.cursor/mcp.json` (Cursor asks to approve it once) |
| `copilot` GitHub Copilot CLI | `.github/hooks/atlas.json`: loaded once the folder is trusted (the first interactive `copilot` asks; `copilot -p` does not) | `.github/mcp.json` (same trust) |
| `opencode` OpenCode | `.opencode/plugins/atlas.ts` | `opencode.json` |
| `droid` Factory Droid | `.factory/settings.json` | `.factory/mcp.json` |
| `antigravity` Antigravity (`agy`) | `.agents/hooks.json` (run `agy` with an absolute `--add-dir`) | `.agents/mcp_config.json` |
| `pi` Pi | `.pi/extensions/atlas/index.ts`: loaded once the project is trusted in Pi (`pi --approve` for one run) | `.pi/mcp.json` (same trust) |

| command | what |
|---|---|
| `atlas agents add \| remove \| list \| sessions` | install or remove the hook and memory entry per agent; list agents and the sessions seen |
| `atlas serve [--daemon \| --stop]` | the Project's listener: memory server, sync drain, receiver of hook turns while it holds the store |
| `atlas status` | sign-in, Project, gates, capture health, commits linked, sync, memory, agents and hook errors |
| `atlas memory browse \| list \| verify \| export \| import` | shared memory (this machine only) |
| `atlas capture sync \| retry \| link` | cloud sync now, retry failed rows, link sessions to commits by hand |
| `atlas mcp-bridge --project <dir> --agent <id>` | the stdio MCP entry the agents' configs hold (serves memory with or without `atlas serve`) |

The ACP path of v0.1 (`atlas run <agent>` with Atlas's own chat screen, the
registry installs) is parked under the hidden `atlas acp` group (ADR-0013 §8).

## Where things are

| path | what |
|---|---|
| `docs/atlas-reference/README.md` | index of the hand-off folder copied from the desktop repo: read first |
| `docs/atlas-reference/SPEC.md` | the specification; §1.2 holds the locked decisions D1–D20 |
| `docs/atlas-reference/evidence-*.md` | desktop-side facts with `file:line` citations into the desktop repo |
| `docs/research/entire-cli.md` | how the Entire CLI captures agents through hooks; the per-agent facts this CLI's integrations follow |
| `docs/adr/` | this repo's own ADRs; `0013` is the hook-based capture of native agent UIs and supersedes `0009`/`0012` as the product |
| `docs/RELEASE.md` | how to release, and the acceptance checklists |
| `docs/progress.md` | what was built, what was verified live, what was not |

Ground rules that are not negotiable: a hook reads only the transcript file
the agent's own payload names, from the turn's offset, while the hook runs
(never a scan of an agent's directories); never inject memory into prompts;
never log an `Authorization` header; every stored text passes the redactor
first; and no secret persists except the session token file (mode 0600).
