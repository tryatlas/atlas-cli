# Releasing the Atlas CLI

v1 ships for **linux/amd64, linux/arm64 and darwin/arm64** (D18: no
Windows). It is pure Go (`CGO_ENABLED=0`), so every target cross-compiles
from one machine. Intel Macs build from source (`make build`).

## What a release is

- A `v*` tag on `main`. `.github/workflows/release.yml` runs the tests,
  then GoReleaser (`.goreleaser.yaml`) builds the three archives
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
5. When the release workflow is green, review the draft release (three
   archives, `checksums.txt`, `install.sh`), then publish it.
6. On a clean Linux and a clean Apple-silicon Mac:
   `curl -fsSL …/install.sh | sh`, then `atlas --version`.

## Acceptance checklists

Run them before tagging. Record the results in `docs/progress.md`.

### A. CLI smoke (any machine, about 10 minutes)

| # | Step | Expect |
|---|---|---|
| 1 | `atlas --version` | The tag's version |
| 2 | `atlas login`, then `atlas whoami` | Signed in; organisations listed |
| 3 | In a scratch git repo: `atlas init --local`, then `atlas status` | Project, gates on, "Sync: off (local Project)", "Record: … this machine only" |
| 4 | `atlas agents list` | The registry, nothing installed on a fresh machine |
| 5 | `atlas agents check claude-acp` | protocol 1, `session: ok` or `sign-in required`, the sign-in methods |
| 6 | `atlas run claude-acp --no-tui --yes --prompt "Create hello.md with one line"` | The reply is printed; `hello.md` exists |
| 7 | `atlas run claude-acp` (chat screen): a prompt that edits a file, then ctrl+t, ctrl+p, ctrl+l, ctrl+c | Permission prompt with the agent's own options; a tool card with its diff; the mode and model pickers; the session list; a clean exit |
| 8 | `atlas memory browse` and `atlas memory list` | The entries the agent wrote (file_changed, any decision) |
| 9 | Commit the file, then `atlas capture link` | "1 checkpoint(s) written" |
| 10 | `git commit --amend -m reworded`, then `atlas capture link` | "Rewritten history: 1 relinked" |
| 11 | `atlas status` | Sessions, "Commits: linked up to <sha>", agents listed |
| 12 | `atlas serve --daemon`, `atlas status` (recorder held), `atlas serve --stop` | Starts, held, stops |

### B. Cloud sync (a throwaway cloud Project)

| # | Step | Expect |
|---|---|---|
| 1 | `atlas init --org <id> --create --slug cli-acceptance-<date> --yes` | Bound, "cloud: <org> / <slug>" |
| 2 | One `atlas run … --prompt` that edits a file; commit; `atlas capture link` | — |
| 3 | `atlas capture sync`, then `atlas status` | "Sync: ok · N sent, 0 pending, 0 failed" |
| 4 | Open the Project in the web app or a teammate's desktop | The session with its tool calls, and the checkpoint |

### C. Desktop parity

`docs/acceptance/acp.md`: the folder opened in the Atlas desktop app shows
the CLI's sessions exactly as if the desktop had recorded them (titles, tool
calls, file touches, no duplicates, usage, checkpoints, memory handoff,
cloud Timeline).

### D. Agents

`atlas agents check --all` on each release platform. Every agent with a
build for the platform must install, launch and answer `initialize`. A
failure is either an agent problem (recorded in `docs/progress.md` with its
wire evidence, `ATLAS_ACP_TRACE`) or a CLI bug (fixed before the tag).
