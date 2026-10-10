# Desktop acceptance checklist: an ACP session captured by `atlas run`

Checks that a session driven by `atlas run <agent>` opens in the Atlas
desktop app, and reaches the cloud Project, as if the desktop had run it.

**Folder:** `~/Codes/atlas-test-cloud`, bound to the cloud Project
`atlas-cli-acceptance` in the "Atlas org" organisation (id
`01M4KNA93EHF0J98GEC08F6JZ2`). It holds three `atlas run claude-acp`
sessions:

| session (ACP id) | what it did |
|---|---|
| `6dd23abc…` "Create a file acp-notes.md containing…" | **Write** `acp-notes.md` |
| `bafef75e…` "Append the line 'second line from atlas run'…" | **Bash** (`echo … >> acp-notes.md`) |
| `09b381f2…` "Use your file editing tool…" | **Read**, ToolSearch, **Edit** `acp-notes.md`, `atlas_memory` `memory_remember` |

Commit `a38e248` "Add ACP notes from atlas run" carries the first two
edits. The third edit (`third line, edited`) is **not committed yet**: row 6
commits it.

**Before you start:** make sure no `atlas run` or `atlas serve` is running
there. While one holds `.atlas/sessions.lock`, the desktop opens the folder
read-only ("Another Atlas window is recording this Project").

## 1. Reference numbers (what the CLI wrote)

```sh
cd ~/Codes/atlas-test-cloud
sqlite3 .atlas/sessions.db "select native_session_id, title from agent_session where agent='claude-acp' order by started_at;"
sqlite3 .atlas/sessions.db "select s.native_session_id, t.tool_name, t.title, t.status from tool_call t join agent_session s on s.id=t.session_id where s.agent='claude-acp' order by t.seq;"
sqlite3 .atlas/sessions.db "select sum(input_tokens), sum(output_tokens), sum(cache_creation_tokens), sum(cache_read_tokens) from usage_delta u join agent_session s on s.id=u.session_id where s.agent='claude-acp';"
```

At the time of writing the usage sums were input 14, output 906, cache
creation 55,942, cache read 164,866.

## 2. In the desktop app

Open `~/Codes/atlas-test-cloud`, then the Timeline.

| # | Check | Expect | Result |
|---|---|---|---|
| 1 | Titles | The three sessions above, titled from their first prompt (120 characters, ending in "…" where cut) | |
| 2 | Tool calls | `6dd23abc`: **Write** "Write acp-notes.md". `bafef75e`: **Bash** with the `tail …; echo … >> acp-notes.md` command. `09b381f2`: **Read**, **Other** (ToolSearch), **Edit**, **Other** (`mcp__atlas_memory__memory_remember`). Each shows completed, with its arguments; the Write and the Edit show their patch | |
| 3 | File touches | `acp-notes.md` under the Write (new file) and under the Edit (existing file). The Bash call has **none**: the file was already untracked before the command ran, and the desktop's shell rule (git status after minus before) gives the same result | |
| 4 | No duplicate | Each session appears **once**. claude-agent-acp also writes Claude Code's own transcript; the desktop importer should skip it because it carries the same native session id. Note any extra "imported" rows and their titles | |
| 5 | Usage | The three sessions' totals match step 1's sums | |
| 6 | Checkpoint after a commit | Commit `a38e248` shows under `6dd23abc`. Then commit the third edit (`git commit -am "Third line"`), reopen the Timeline (or run `atlas capture link` first; note which), and the new commit shows under `09b381f2` | |
| 7 | Memory handoff to a desktop-run agent | In the desktop, start any agent in this folder and ask it to call `memory_briefing` and `memory_why acp-notes.md`. Expect the decision "acp-notes.md lines are appended, never rewritten." (session `09b381f2`, agent `claude-acp`), `acp-notes.md` among recent file changes, and the sessions that wrote the file | |
| 8 | Memory panel | The same decision in the Memory panel; Health shows a **green** chain | |
| 9 | Cloud Project | In the web app (or a teammate's desktop) open the Project **Atlas CLI acceptance (throwaway)**: the three sessions are on its Timeline with their tool calls, and the `a38e248` checkpoint | |
| 10 | Capture status | The capture popover shows capture **on**, Cloud, no "Another Atlas window" message | |

## 3. Report back

For each row: ✅, or ❌ with what the desktop showed (a screenshot is
fine). Anything ❌ becomes a fix before the rest of Phase 7.
