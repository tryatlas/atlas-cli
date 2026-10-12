# Releasing the Atlas CLI

v1 ships for **linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64**
(D18 and ADR-0011: no Windows). It is pure Go (`CGO_ENABLED=0`), so every
target cross-compiles from one machine, and CI tests each on a native
runner.

## What a release is

- A `v*` tag. `.github/workflows/release.yml` runs the tests, then
  GoReleaser (`.goreleaser.yaml`) builds the four archives
  `atlas_<version>_<os>_<arch>.tar.gz` (binary plus README),
  `checksums.txt` (sha256) and `install.sh`, and opens a **draft** GitHub
  release with them.
- The version comes from the tag: GoReleaser sets `main.version` with
  `-X main.version={{ .Version }}`, so `atlas --version` prints
  `atlas 1.2.3`. Local builds use `git describe` (`make build`).
- `install.sh` picks the archive for the machine, verifies its sha256
  against `checksums.txt`, and installs to `~/.local/bin`
  (`ATLAS_INSTALL_DIR`, `ATLAS_VERSION` and `ATLAS_BASE_URL` override).

```sh
curl -fsSL https://github.com/tryatlas/atlas-cli/releases/latest/download/install.sh | sh
```

## Steps

1. On `main`, everything green: `make fmt lint test`, and CI on the last
   commit (`gh run list --branch main`).
2. `make release-check` (GoReleaser validates the config) and
   `make snapshot` (builds the archives into `dist/` without publishing).
   Check `dist/checksums.txt` and test the installer against them:

   ```sh
   ATLAS_VERSION=v0.0.1-snapshot ATLAS_BASE_URL="file://$PWD/dist" ATLAS_INSTALL_DIR=/tmp/atlas-test sh install.sh
   ```
3. Run the acceptance checklists below on a real machine.
4. Tag and push: `git tag -a v1.0.0 -m "Atlas CLI 1.0.0" && git push origin v1.0.0`.
5. When the release workflow is green, review the draft release (four
   archives, `checksums.txt`, `install.sh`), then publish it.
6. On a clean Linux machine and on a Mac (Apple silicon or Intel):
   `curl -fsSL …/install.sh | sh`, then `atlas --version`.

## Acceptance checklists

Run them before tagging. Record the results in `docs/progress.md`.

### A. CLI smoke (any machine, about 10 minutes)

| # | Step | Expect |
|---|---|---|
| 1 | `atlas --version` | The tag's version |
| 2 | `atlas login`, then `atlas whoami` | Signed in; organisations listed |
| 3 | In a scratch git repo: `atlas init --local`, then `atlas status` | Project, gates on, "Sync: off (local Project)", "Record: … this machine only" |
| 4 | `atlas agents list` | The eight agents; hook and memory "absent"; binaries on PATH named |
| 5 | `atlas agents add claude-code` | "hook installed, memory server added to .mcp.json"; the git post-commit hook installed; `.claude/settings.local.json` holds six Atlas hooks |
| 6 | `claude -p "Create hello.md with one line"` | The reply; `hello.md` exists; `atlas status` shows 1 session; `atlas agents sessions` lists it with `claude -r <id>` |
| 7 | `claude` (its own UI): a prompt that edits a file and runs a command, then exit | The SessionStart banner "Atlas: capture on · memory on · …"; while the agent still works, `sqlite3 .atlas/sessions.db "select tool_name from tool_call"` already lists the calls (per-event); after Stop one completed turn with the prompt first, one row per call, the reply last, no `changes-N` call |
| 8 | `atlas memory list` | `file_changed` entries from both turns |
| 9 | Commit the file | The post-commit hook links it: `atlas status` says "Commits: linked up to <sha>" without running `atlas capture link` |
| 10 | `git commit --amend -m reworded`, then `atlas capture link` | "Rewritten history: 1 relinked" |
| 11 | `atlas serve --daemon`, one more `claude -p` turn, `atlas serve --stop` | The turn is recorded through the listener (no hook error in `atlas status`) |
| 12 | `atlas agents remove claude-code` | Hooks and the memory entry gone; the git hook gone when no agent is left |

### B. Cloud sync (a throwaway cloud Project)

| # | Step | Expect |
|---|---|---|
| 1 | `atlas init --org <id> --create --slug cli-acceptance-<date> --yes` | Bound, "cloud: <org> / <slug>" |
| 2 | One `claude -p` (or any hooked agent) turn that edits a file; commit | — |
| 3 | `atlas capture sync`, then `atlas status` | "Sync: ok · N sent, 0 pending, 0 failed" |
| 4 | Open the Project in the web app or a teammate's desktop | The session with its tool calls, and the checkpoint |

### C. Desktop parity

`docs/acceptance/acp.md`: the folder opened in the Atlas desktop app shows
the CLI's sessions exactly as if the desktop had recorded them (titles, tool
calls, file touches, no duplicates, usage, checkpoints, memory handoff,
cloud Timeline).

### D. Agents

`atlas agents add <agent>` then one turn in each agent's own UI, on each
release platform, for every agent installed on the machine. The turn must
land in `atlas status` with its tool calls and file touches, and `atlas
agents sessions` must print a resume command that works. A failure is either
an agent problem (recorded in `docs/progress.md` with the payload shape) or
a CLI bug (fixed before the tag).
