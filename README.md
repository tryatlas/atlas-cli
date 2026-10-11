# atlas-cli

A headless Atlas for servers: a Go binary with a Bubble Tea TUI that signs in
like the Atlas desktop app, binds a folder to an organisation and a cloud
Project, and gives every coding agent run in that folder **shared memory** and
**session capture**, byte-compatible with the desktop's on-disk formats.

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
atlas login                 # device grant against the Atlas auth server
cd ~/work/backend
atlas init                  # org + cloud Project, writes .atlas/ (--local: no cloud)
atlas agents list           # the ACP registry: every agent, installed or not
atlas run claude-acp        # chat screen: any registry agent over ACP, with
atlas run codex-acp         #   full capture, cloud sync and shared memory
atlas status
```

| command | what |
|---|---|
| `atlas run <agent>` | chat screen (streamed replies, tool cards with diffs, permission prompts, mode/model pickers, slash commands, sessions; ctrl+h lists the keys) |
| `atlas run <agent> --prompt "…" --no-tui [--yes] [--image f.png]` | one prompt, headless, for CI and cron |
| `atlas agents list \| install \| uninstall \| detect \| check \| logout` | the registry, installs, and a handshake check (`check --all`) |
| `atlas memory browse \| list \| verify \| export \| import` | shared memory (this machine only) |
| `atlas capture sync \| retry \| link` | cloud sync now, retry failed rows, link sessions to commits |
| `atlas serve [--daemon \| --stop]` | the Project's listener without a chat screen |
| `atlas status` | sign-in, Project, gates, capture health, sync, memory, agents |

Agents sign in through their own methods (`atlas run` offers them when an
agent asks). Debugging an agent: `ATLAS_ACP_TRACE=/tmp/acp.trace atlas run …`
records the shape of every protocol message, never its content.

## Where things are

| path | what |
|---|---|
| `docs/atlas-reference/README.md` | index of the hand-off folder copied from the desktop repo: read first |
| `docs/atlas-reference/SPEC.md` | the specification; §1.2 holds the locked decisions D1–D20 |
| `docs/atlas-reference/evidence-*.md` | desktop-side facts with `file:line` citations into the desktop repo |
| `docs/atlas-reference/adr/` | the desktop ADRs this project leans on |
| `docs/adr/` | this repo's own ADRs; `0000` records D7, `0001`–`0007` record D13–D19, `0009` records D20 (ACP only, the app's way), `0010` the hand-rolled JSON-RPC, `0011` the Intel Mac release; `0000`, `0003`, `0004` are superseded |
| `docs/RELEASE.md` | how to release, and the acceptance checklists |
| `docs/progress.md` | what was built, what was verified live, what was not |

Ground rules that are not negotiable: never read another agent's transcript
files, never inject memory into prompts, never log an `Authorization` header,
every stored text passes the redactor first, and no secret persists except the
session token file (mode 0600).
