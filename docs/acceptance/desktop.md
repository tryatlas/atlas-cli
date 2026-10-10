# Desktop acceptance checklist (Phase 6b)

> **Parked with the proxy path (ADR-0009).** The sessions in
> `~/Codes/atlas-test-3` were captured by `atlas shell`, which now lives on
> the `parked/proxy-path` branch. The store-level checks (rows 1–6, 8–10)
> still describe what a CLI-written store must look like; row 7 needs that
> branch. The ACP acceptance checklist replaces this one in Phase 7.

Checks that a folder captured by the Atlas CLI opens in the Atlas desktop app
as if the desktop had recorded it. Run it on `~/Codes/atlas-test-3`, which holds
three CLI-captured Claude Code sessions (two `claude -p`, one interactive),
linked to commit `4b3c934`.

**Before you start:** quit any `atlas shell` in that folder. While the CLI
holds `.atlas/sessions.lock`, the desktop opens the folder read-only and says
"Another Atlas window is recording this Project".

## 1. What the CLI recorded (reference numbers)

Run these in a terminal so you have numbers to compare against:

```sh
cd ~/Codes/atlas-test-3
sqlite3 .atlas/sessions.db "select title from agent_session order by started_at;"
sqlite3 .atlas/sessions.db "select count(*) from tool_call; select count(*) from file_touch;"
sqlite3 .atlas/sessions.db "select sum(input_tokens), sum(output_tokens), sum(cache_creation_tokens), sum(cache_read_tokens) from usage_delta;"
sqlite3 .atlas/sessions.db "select commit_sha, files_touched from checkpoint;"
```

## 2. In the desktop app

Open `~/Codes/atlas-test-3` (File ▸ Open Folder), then open the Timeline.

| # | Check | Expect | Result |
|---|---|---|---|
| 1 | The session list | The three CLI sessions from step 1: "Create notes.md with a heading # Notes…", "Append a line: third line to notes.md…", "Reply with exactly the word: pong" | |
| 2 | Open "Create notes.md…" | The prompt, Claude's reply, and tool calls **Write**, **Edit**, **Other (mcp__atlas__memory_search)** with their results | |
| 3 | File touches in that session | `notes.md`, twice (one per tool), with the edit patches `+# Notes…` and `-first line / +updated line` | |
| 4 | Duplicates | Each CLI session appears **once**. Up to three more may appear as imported from Claude's own history: my earlier test runs whose CLI rows I reset ("Create a file named hello.txt…", "Create a file named hello2.txt…", a first "Create notes.md…"). Note how many and their titles | |
| 5 | Usage tab | The CLI sessions' totals match step 1's sums (at the time of writing: input 16, output 1,191, cache creation 239,963, cache read 488,649; imported sessions add their own) | |
| 6 | Checkpoints | Commit `4b3c934` "Add notes from the agent" shows under the two notes.md sessions | |
| 7 | New commit | Make one more change through Claude in `atlas shell` (e.g. "append a line to notes.md"), exit the shell, commit it, run `atlas capture link`, then reopen: the new commit appears as a checkpoint of that session. (Skipping `capture link` also works: the desktop links on open. Note which one you did) | |
| 8 | Capture status | The capture popover shows capture **on**, Local, no "Another Atlas window" message | |
| 9 | Memory panel | The decision "notes.md lines are appended, never rewritten." and the fact about `hello.txt` (marked stale: the file is gone); Health shows a **green** chain | |
| 10 | Memory provenance | The decision names session `…5d908253…` (the same id as the "Append a line…" capture session) | |

## 3. Report back

For each row: ✅, or ❌ with what the desktop showed (a screenshot is fine).
The CLI-side fixes are in Phase 6b; anything ❌ here becomes a fix before 7a.
