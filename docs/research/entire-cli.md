# How the Entire CLI works

Research notes on `github.com/entireio/cli` at commit `1f44a6f` (2026-10-09,
v0.11.4 plus a few days), read on 2026-10-12 from a shallow clone. Entire is the
model for Atlas CLI's next direction: hook-based capture of sessions run in each
agent's own UI, "same as Entire, but the Atlas way". Paths are relative to the
Entire repo, mostly under `cmd/entire/cli/`.

Size: 1,652 Go files, ~321k lines of non-test Go, of which `internal/coreapi`
is ~107k lines of generated client. The capture core is about 65k lines:
`agent/` 28.5k, `strategy/` 23.4k, `checkpoint/` 11.4k, `session/` 1.9k,
`redact/` 3.3k, `agentimport/` 2.1k. Go 1.27.1, cobra, Charm (bubbletea v2,
huh v2, lipgloss v2), go-git v6, betterleaks, go-keyring, posthog. MIT.

## 1. The one-paragraph version

`entire enable` writes a hook entry into each agent's own config file in the
repo. The agent then runs `entire hooks <agent> <event>` at each lifecycle
point with a JSON payload on stdin. Entire normalises eight agents' hooks into
one event set (SessionStart, TurnStart, TurnEnd, Compaction, SessionEnd,
SubagentStart, SubagentEnd, ToolUse) and one dispatcher. At turn start it
snapshots `git status` and the transcript's line offset; at turn end it reads
the agent's transcript file from that offset, extracts prompts, files edited,
tokens and model, merges a git-status diff, and records everything in session
state under `.git/entire-sessions/`. Nothing touches git until the user
commits: a `prepare-commit-msg` hook adds an `Entire-Checkpoint: <id>` trailer
and `post-commit` condenses the pending sessions into a git ref
`refs/entire/checkpoints/<shard>/<id>` holding `metadata.json` and the redacted
transcripts. `pre-push` pushes those refs with the user's own push.

## 2. Agent framework

### 2.1 Interfaces (`agent/agent.go`, `agent/capabilities.go`)

Core `Agent`: `Name` (registry key, e.g. `claude-code`), `Type` (display, e.g.
`Claude Code`), `Description`, `DetectPresence`, `ProtectedDirs`,
`ReadTranscript`, `ChunkTranscript`, `ReassembleTranscript`, `GetSessionID`,
`GetSessionDir`, `ResolveSessionFile`, `ReadSession`, `WriteSession`,
`FormatResumeCommand`.

Optional interfaces, resolved by `As*` helpers (external plugins must also
declare the capability): `HookSupport` (install/uninstall/are-installed +
`HookNames`, `ParseHookEvent`), `HookFreshness` (Absent/Current/Outdated),
`StaleHookReporter`, `TranscriptAnalyzer` (`GetTranscriptPosition`,
`ExtractModifiedFilesFromOffset`), `PromptExtractor`, `TranscriptPreparer`
(flush wait), `LateTranscriptWriter` (Antigravity), `TranscriptFetcher`
(OpenCode export), `SidecarImageProvider` (Cursor SQLite), `TranscriptSanitizer`
(Codex encrypted blobs), `TokenCalculator`, `OutOfBandTokenSource`,
`ModelExtractor`, `TextGenerator` (summaries), `HookResponseWriter` (banner),
`ContextInjector` (text for the model), `SessionEndBudgeter` (Codex 3 s),
`SubagentAwareExtractor`, `SubagentSessionResolver` (Droid Workers),
`HomeLayoutProvider` (relocatable home), `CallerSessionIdentifier` (env var
naming the session), `SkillEventExtractor`, `ToolInvocationScanner`.

`Event` (`agent/event.go:83-242`): `Type`, `SessionID`, `PreviousSessionID`,
`SessionRef` (transcript path), `Prompt`, `Model`, `Timestamp`, `ToolUseID`,
`TurnID`, subagent fields (`SubagentID`, `Final`, `SubagentLaunch`,
`SubagentTranscriptPath`, `ToolInput`, `SubagentType`, `TaskDescription`),
file lists (`ModifiedFiles`, `NewFiles`, `DeletedFiles`, `CWD`),
`ResponseMessage`, metrics (`DurationMs`, `TurnCount`, `ContextTokens`,
`TokenUsage`), `SkillEvents`, `Metadata map[string]string`.

Stdin is read with a streaming `json.Decoder`, never `io.ReadAll`, so a hook
returns after one JSON value even if the agent never closes the pipe
(`event.go:270-324`, Windows/Git Bash hang #1398). A TTY on stdin is "empty
hook input".

### 2.2 The hook command and its wrapper (`agent/hook_command.go`)

Every installed command names the `entire` binary, never a path from the repo.
POSIX wrapper: `sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi;
exec entire hooks <agent> <verb>'`. SessionStart uses a variant that prints
`{"systemMessage":"Entire CLI is enabled but not installed or not on PATH…"}`
when the binary is missing. Windows: `cmd.exe /d /s /c "where.exe entire >nul
2>nul & if errorlevel 1 (ver>nul) else (<cmd>)"`, chosen either by a probe
(`sh -c 'exit 0'` fails under cmd.exe; Codex, Cursor) or unconditionally on
Windows (Droid, Antigravity, whose runners never use sh).

`IsManagedHookCommand` matches the prefix `entire hooks ` (not just `entire `,
so a user's own `entire status` hook is never deleted) plus four legacy
local-dev shapes, bare or wrapped. Every install, with or without `--force`,
prunes managed entries whose command is not in the wanted set
(`DropStaleManagedHooks`); this exists because a retired dev launcher once ran a
script from the working tree on every turn.

### 2.3 Dispatcher (`lifecycle.go`, `hook_registry.go`)

`entire hooks <agent> <verb>` is a hidden command tree. Before dispatch: not a
git repo → exit 0; `!settings.IsSetUpAndEnabled` → exit 0 (any settings read
error counts as disabled); a hook whose `transcript_path` lies inside another
agent's session directory is dropped (Cursor forwarding, `hook_guard.go`); for
events other than SessionStart/TurnStart, a recorded `state.AgentType` that
differs from the firing agent means skip. Session, tool-use and subagent IDs
are validated as path-safe centrally.

Per event:

- **SessionStart.** Writes `.git/entire-sessions/<id>.agent` (O_EXCL, first
  writer wins), a `.model` hint, a trails-enabled cache refresh (1 s). Builds the
  banner "Entire CLI will link this conversation to your next commit." (+ "N
  other active conversation(s)…"), printed only by agents with
  `HookResponseWriter` and only by the winner of an O_EXCL `.banner` claim.
  Output: `{"systemMessage":"…"}`. Spawns a detached `__sweep_sessions` child
  if zombie sessions exist (throttled to one per 15 min per repo).
- **TurnStart.** Lock waits bounded to 2 s. `EnsureSetup` (gitignore, orphan
  ref, git hooks reinstall if missing). `CapturePrePromptState` →
  `.entire/tmp/pre-prompt-<id>.json` with untracked files (fail-open; records
  `untracked_scan_skipped`), `transcript_offset` (line count), optional token
  baseline. Appends the prompt to `.entire/metadata/<id>/prompt.txt`.
  `InitializeSession` creates/updates state: `BaseCommit=HEAD`, new `TurnID`,
  `LastPrompt`, `Branch`, `Owner` (pid + start time), `TranscriptPath`. Once per
  session, stdout gets `{"hookSpecificOutput":{"hookEventName":
  "UserPromptSubmit","additionalContext":"Entire is enabled… run `entire
  agent-help`"}}`, gated on a durable state write so it never double-injects.
- **TurnEnd.** `PrepareTranscript` (Claude: poll ≤ 3 s for the `hook_progress`
  sentinel or a 500 ms size-quiet window; OpenCode: run `opencode export`).
  Missing transcript → exit 1. Sanitize and write
  `.entire/metadata/<id>/full.jsonl` (0600). Offset = pre-prompt offset, else
  `state.CheckpointTranscriptStart`. Modified files from the transcript (Claude:
  `Write`, `Edit`, `NotebookEdit`, `mcp__acp__Write/Edit`, path from
  `input.file_path`/`notebook_path`; subagent transcripts included). New files =
  untracked now minus untracked at turn start; deleted from git status. Paths
  normalised to repo root; files identical to HEAD dropped; gitignored and
  submodule paths dropped. Tokens: hook payload wins, else transcript (Claude:
  group assistant rows by `message.id`, keep the highest `output_tokens`,
  sum input/cache_creation/cache_read/output; `APICallCount` = unique ids).
  `SaveStep` updates state only: `StepCount++`, `FilesTouched` ∪, per-file blob
  hashes via `git hash-object`, token accumulation. Then `HandleTurnEnd`
  rewrites any mid-turn checkpoint with the full transcript. Nothing on stdout.
- **Compaction.** State transition only. Claude installs no PreCompact hook.
- **SessionEnd.** Finalise in-flight background tasks, mark ENDED, reserve a
  checkpoint id, then condense eagerly (fail-open). Codex marks ended first
  because its host kills the hook at 3 s.
- **SubagentStart/End.** Pre-task untracked baseline per `tool_use_id`;
  completion attaches files (transcript + baseline diff) and tokens to a
  `TaskRecord` exactly once. Claude's `SubagentStop` has no `tool_use_id`, so
  the record is matched by agent id.

Exit codes: silent 0 for every "not our business" path; 1 (non-blocking for
Claude) only on parse failures, invalid ids, missing transcript, write
failures. Exit 2 (blocking) is never produced. Only SessionStart and the
one-time TurnStart injection write stdout.

### 2.4 Claude Code hook config as written

Ten entries across nine hook types in `.claude/settings.json`, no timeouts,
merged into a raw map so unknown keys and user hooks survive; written with
2-space indent, 0600, atomically; keys come out alphabetical:

| hook type | matcher | verb |
|---|---|---|
| SessionStart | `""` | session-start (JSON-warning wrapper) |
| UserPromptSubmit | `""` | user-prompt-submit |
| Stop | `""` | stop |
| StopFailure | `""` | stop-failure (API error ends the turn, 0.11.4) |
| SessionEnd | `""` | session-end |
| PreToolUse | `Agent` | pre-task |
| PostToolUse | `Agent`, `Skill` | post-task |
| SubagentStart | `workflow-subagent` | subagent-start |
| SubagentStop | `""` | subagent-stop |

Payload fields read: `session_id`, `transcript_path`, `model`, `prompt`
(IDE tags stripped), `tool_use_id`, `tool_input`, `tool_name`,
`tool_response.{agentId,status,isAsync,background}`, `agent_id`,
`agent_type`, `agent_transcript_path`. Subagent transcripts:
`<transcript_dir>/<session_id>/subagents/agent-<agent_id>.jsonl`.

Prompts kept from the JSONL: `type/role == user`, not `isMeta`,
`isCompactSummary` or `isSidechain`, text blocks only, minus injected prompts,
`<task-notification>`, `[Request interrupted by user`, `<bash-input>`,
`<bash-stdout>`, `<bash-stderr>`.

## 3. The other seven agents

| agent | binary | hook file | shape | events → Entire | transcript |
|---|---|---|---|---|---|
| Codex ≥ 0.124 | `codex` | `.codex/hooks.json` | Claude-like groups, `matcher: null`, `timeout` 30 (SessionEnd 3) | SessionStart, SessionEnd, UserPromptSubmit→TurnStart, Stop→TurnEnd, PostToolUse[apply_patch]→ToolUse, SubagentStart/Stop | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` (+`archived_sessions`); `session_meta`/`response_item`/`event_msg`/`turn_context`; files from `apply_patch` envelopes and `FileChange` items; tokens from cumulative `token_count` deltas |
| Cursor CLI/IDE | `agent` | `.cursor/hooks.json` | `{"version":1,"hooks":{ev:[{"command"}]}}` | sessionStart, beforeSubmitPrompt→TurnStart, stop→TurnEnd (tokens in payload), sessionEnd, preCompact→Compaction, subagent* | `~/.cursor/projects/<slug>/agent-transcripts/<id>.jsonl` (CLI flat, IDE nested); `CURSOR_DATA_DIR` ignored on purpose; files from `Write`/`StrReplace`; images in SQLite `store.db` |
| Copilot CLI 1.0.82 | `copilot` | `.github/hooks/entire.json` (own file; dir auto-discovered) | `{"version":1,"hooks":{ev:[{"type":"command","bash":…}]}}` | all nine: userPromptSubmitted→TurnStart, sessionStart, agentStop→TurnEnd, sessionEnd, subagent*, pre/postToolUse, errorOccurred | `$COPILOT_HOME/session-state/<id>/events.jsonl`; files from `tool.execution_complete` telemetry `filePaths`; tokens from `session.shutdown` `modelMetrics` |
| Factory Droid | `droid` | `.factory/settings.json` | Claude-like, matcher `""`/`Task` | SessionStart (two entries so TurnStart fires under `droid exec`), UserPromptSubmit, Stop, SessionEnd, PreCompact, Pre/PostToolUse[Task] | `$FACTORY_HOME_OVERRIDE/.factory/sessions/<slug>/<id>.jsonl`; Workers run as detached sessions linked by `callingSessionId` |
| Antigravity (`agy`) | `agy` | `.agents/hooks.json` under key `"entire"` | `PreToolUse[*]`, `PreInvocation`, `Stop` (timeout 300) | PreInvocation→TurnStart, PreToolUse→ToolUse, Stop(fullyIdle)→TurnEnd; no session events | `~/.gemini/antigravity-cli/brain/<conv>/.system_generated/logs/transcript_full.jsonl`, written **after** Stop; tokens only via a global `title` tee into the user cache |
| OpenCode | `opencode` | `.opencode/plugins/entire.ts` (owned file, embedded) | TS plugin spawning `entire hooks opencode …` | session.created→SessionStart, message.part.updated(user)→TurnStart, session.status idle→TurnEnd (sync), session.compacted, session.deleted/server.instance.disposed→SessionEnd | no native file read: `opencode export <id>` into `.entire/tmp/<id>.json`, one JSON document |
| Pi | `pi` | `.pi/extensions/entire/index.ts` | TS extension, `execFile sh -c`, 10 s timeout, no `command -v` guard | session_start, before_agent_start→TurnStart (injects context as a hidden message), agent_end→TurnEnd; session_shutdown ignored (races agent_end) | `$PI_CODING_AGENT_DIR/sessions/<slug>/<ts>_<id>.jsonl`, tree-shaped (`parentId`), only the active branch is read |

Gemini CLI support was removed in 0.11.3; Antigravity is its successor.
Known per-agent gaps: Cursor `-p` mode fires no beforeSubmitPrompt/stop;
Codex `transcript_path` is null in `--ephemeral`, no hooks under `-s
read-only`, hooks without a `trusted_hash` in `~/.codex/config.toml` are
silently skipped; Copilot, OpenCode and Pi have no Windows wrapper; Antigravity
in `-p` leaves ghost ACTIVE sessions. Doc drift found: Codex AGENT.md says
`rollouts/`, code uses `sessions/`; AGENT.md claims an exact 0.149.0 pin, CI
installs latest.

External agents: binaries named `entire-agent-<name>` on absolute `$PATH`
entries, discovered only when `external_agents: true` sits in an untracked
`.entire/settings.local.json`. Stateless subcommand protocol (`info`, `detect`,
`parse-hook --hook`, `install-hooks`, `read-transcript`, `extract-modified-files
--offset`, `calculate-tokens`, …), JSON over stdin/stdout, `ENTIRE_PROTOCOL_VERSION=1`,
30 s timeout, `generate-text` runs in an empty temp dir without `GIT_*`.

## 4. Session state and the manual-commit strategy

### 4.1 State (`session/state.go`)

One JSON per session at `<git-common-dir>/entire-sessions/<id>.json` (0600,
atomic, shared across worktrees), plus a per-session flock in
`entire-session-locks/<id>.lock` (never unlinked). Key fields: `SessionID`,
`BaseCommit` (follows HEAD), `WorktreePath/ID`, `Branch`, `Phase`
(active/idle/ended), `Kind` (agent_review/agent_investigate/imported),
`TurnID`, `TurnCheckpointIDs`, `LastInteractionTime`, `Owner` (pid+start),
`StepCount`, `CheckpointTranscriptStart` (offset in the agent's unit),
`CheckpointTranscriptSize` (sanitized bytes at last condense),
`FilesTouched`, `TouchedFileHashes` (`""` = deleted), `LastCheckpointID`,
`CondensationAttempt{CheckpointID, RecoveryPending}`, `AgentType`,
`ModelName`, `TranscriptPath`, `AgentHome`, `LastPrompt`, `TokenUsage` (session
total), `CheckpointTokenUsage` (delta), `TaskRecords[]`, `SubagentInventory`,
`SkillEvents`, `FullyCondensed`, `ContextInjectionDecided`.

Phase machine (`session/phase.go`): TurnStart → ACTIVE; TurnEnd → IDLE;
SessionStop → ENDED; TurnStart from ENDED → ACTIVE (clears `EndedAt`);
GitCommit triggers Condense (ACTIVE/IDLE) or CondenseIfFilesTouched /
DiscardIfNoFiles (ENDED), nothing during a rebase. Loads purge state with no
interaction for 7 days (even with pending work; imported sessions exempt).
`MutateSessionState` is the only safe mutation path: flock → load → fn → save,
re-entrant per goroutine, post-save effects run outside the lock. Condensation
holds the lock for its whole run (~30 s observed on large sessions); TurnStart
gives up after 2 s and repairs later.

### 4.2 Git hooks installed (`strategy/hooks.go`)

`prepare-commit-msg`, `commit-msg`, `post-commit`, `post-rewrite`, `pre-push`.
Each is `#!/bin/sh` + marker `# Entire CLI hooks` + `if command -v entire …;
then entire hooks git <name> "$@"; else :; fi`, ending in `|| true` except
pre-push (so an OPF failure can abort a push; bypass `ENTIRE_OPF=no`). A
foreign hook is copied to `<hook>.pre-entire` and chained after Entire's part
with an `ENTIRE_CHAINING_<HOOK>` loop guard; a rotated backup becomes
`<hook>.pre-entire.<timestamp>`. pre-commit's wrapper is reclaimed into the
chain; Husky is only warned about. `core.hooksPath` is respected. Install takes
a per-user flock keyed by the hooks dir.

### 4.3 Commit time

**prepare-commit-msg.** Skip during rebase/cherry-pick/revert. Strip trailers
on the local deleted list. Inherit trailers from `SQUASH_MSG` when a staged path
was changed by a squashed commit, and from commits dropped by a `git reset`
(reflog walk, byte-equal staged files). Amend keeps or restores
`LastCheckpointID`. Candidate sessions = exact worktree match ∪ sibling
worktree (if unambiguous; else those active in the last 15 min; else decline
with an `entire session adopt` hint) ∪ sessions whose `Owner` is an ancestor of
the hook process (nearest wins). **Agent fast path:** with no TTY, or
`commit_linking == "always"`, the first ACTIVE session (or IDLE with a live task
record < 24 h) gets a trailer with no content check. Otherwise
`sessionHasNewContent`: task records, transcript growth beyond
`CheckpointTranscriptSize`, and staged files overlapping `FilesTouched` by
name. `commit_linking` default `prompt`: `-m` commits on a TTY ask `[Y]es /
[n]o / [a]lways` on `/dev/tty`; editor commits get the trailer above git's
comment block with a how-to-remove note. Id: reuse a pending
`CondensationAttempt`, else generate (12-hex on git-branch, ULID on git-refs).

**commit-msg.** Strips the trailer if it is the only content so git aborts an
empty commit.

**post-commit.** Target trailer = stamped trailers minus inherited minus
deleted; among several, the last with no checkpoint yet. Per candidate session
under the lock: skip if a different attempt is pending; ACTIVE sessions are
trusted, others need `filesOverlapWithContent`; `CondenseSession`; in the home
worktree set `BaseCommit=HEAD`, reset the window (`StepCount=0`, clear
`CheckpointTokenUsage`, `FilesTouched`, hashes), `CheckpointTranscriptStart =
transcript end`; guest worktrees keep files and `BaseCommit`. Carry-forward:
paths not committed, or committed with a different blob while the worktree
still differs, or pending deletions, stay in `FilesTouched` with
`StepCount=1` and `CheckpointTranscriptStart=0` (so the next checkpoint
carries the whole transcript again); otherwise `.entire/metadata/<id>/` is
cleared. ENDED + nothing left → `FullyCondensed`. Review sessions are force-
ended after one condensation.

**post-rewrite.** Remaps `BaseCommit` for amend/rebase pairs.

**Linking is by file name, not content**: a commit touching any agent-touched
path links, even if the user rewrote the file. Hashes only decide carry-forward.

### 4.4 Condensation (`strategy/manual_commit_condensation.go`)

Transcript = live file, else stored `full.jsonl`; neither → files/prompt-only
checkpoint for late writers, `StepCount > 0` or ended sessions, else error.
Prompts from `prompt.txt` or the transcript from the offset. Tokens scoped to
the window; subagent tokens minus baseline; Copilot rewrites totals from the
full transcript. Skip if no transcript, files or tasks. Pipeline order
**sanitize → externalize images → redact** (sanitize first so images are not
externalized out of dropped items; externalize before redaction because base64
is high-entropy and would be destroyed). Redaction uses an incremental prefix
cache in `<common-dir>/entire-redact-cache` keyed by config fingerprint (a 70
MB Codex rollout cost 67 s per checkpoint before it). Write is `store.Write
(ReservedSession(opts))` so a shared checkpoint never splits across backends.

### 4.5 Checkpoint storage (`checkpoint/`, `api/checkpoint`)

Tree:

```
metadata.json                 CheckpointSummary: cli_version, checkpoint_id, strategy,
                              branch, commit_sha (imports), checkpoints_count,
                              files_touched, sessions[], token_usage, linked_commits[],
                              has_review, has_investigation, imported
<n>/                          session slot, 0-based; same session_id reuses its slot
  metadata.json               session_id, created_at, branch, agent, model, turn_id,
                              save_step_count, files_touched, checkpoint_transcript_start,
                              token_usage (delta), skill_events, session_metrics,
                              summary{intent,outcome,learnings,friction,open_items}, kind
  full.jsonl[.001…]           sanitized+externalized+redacted transcript, 50 MB chunks
  transcript.jsonl            compacted full session (best effort)
  content_hash.txt            sha256 of the full transcript
  prompt.txt                  redacted prompts
  assets/<name>, assets/manifest.json
tasks/<tool_use_id>/task.json, agent-<agent_id>.jsonl
```

git-refs backend (default for new repos): one ref per checkpoint
`refs/entire/checkpoints/<last-2>/<id>`, the commit's tree *is* the checkpoint,
first write orphan, later writes parented, flock + `git update-ref` CAS with 16
jittered retries. Legacy git-branch backend: subtrees `<id[:2]>/<id[2:]>` on
the orphan branch `entire/checkpoints/v1`. Read routing by id shape (ULID →
refs only; hex → primary then branch). Commits signed best-effort when
`commit.gpgsign` is set. Fetch on demand with proven-absence semantics and a
bounded budget inside hooks.

**Push.** pre-push elects one checkpoint remote: `checkpoint_push_remote`
(fail-closed) → captured habit (first push matching the branch's declared
destination, sticky) → `origin` → sole remote → first remote. Other remotes get
no checkpoints unless they reach the same forge repo. git-refs: drain
`entire-checkpoint-push-queue.jsonl`, batch push `ref:ref` fast-forward only,
per-ref fetch+replay recovery, 2 min / 5 consecutive-failure budget, failures
never fail the user's push. `ENTIRE_CHECKPOINT_TOKEN` rewrites SSH to HTTPS and
injects `Authorization: Basic x-access-token:<tok>` via `GIT_CONFIG_*`.

### 4.6 Recovery

Checkpoint id reserved (`BeginCondensationAttempt`) before the durable write at
session end, doctor and eager condense; a retry rewrites the same id into the
same slot. Known gap: a crash between the write and the state save on the
post-commit path mints a duplicate. Zombie sessions (owner process dead) are
finalised by `doctor` and the detached sweep. Doctor can condense without a
commit.

## 5. Redaction and trust

### 5.1 Pipeline (`redact/`)

Byte-range detectors, union of regions, then splice:

1. **Entropy**: token regex `[A-Za-z0-9+_=-]{10,}` (no `/` so paths don't
   match), Shannon > 4.5, JSON-escape guard, exact-shape exemption for
   `toolu_01` + 22 chars.
2. **Scanners**: betterleaks (default on; findings deduped and every
   occurrence replaced) and goredact (default off; serialized by mutex).
   Disabling both is a settings error; goredact-only failure returns
   `ErrScannerDegraded` and the write must fail.
3. **Provider prefixes**: Supabase `sb_secret_`, `sbp_` only (the rest is left
   to betterleaks).
4. **Credentialed URIs** `scheme://user:pass@host`.
5. **Connection strings**: JDBC, DB URLs with `password=` query, keyword DSNs,
   semicolon DSNs; whole string replaced; skipped when there is no `=`.
6. **Custom rules** (inline `custom_redactions` + YAML/JSON packs in
   `.entire/redactors/`, with self-test samples, 1 MiB/256-file caps, symlinked
   dir is an error).
7. **Bounded credential values** `*(db|pg|mysql|…)*(password|passwd|pwd)=…`,
   value only.
8. **PII** (opt-in): email, NANP phone, US address (off), custom labels;
   `[REDACTED_EMAIL]` etc.; bot-email allowlist.
9. **OPF** (OpenAI Privacy Filter, opt-in, push time only): shells out to
   `opf`, batches with `\x1e`, circuit breaker, `Entire-OPF-Applied` trailer,
   fail-closed on git-branch, withholds refs on git-refs.

Secrets become bare `REDACTED`. Placeholders (`${…}`, `<name>`, `changeme`,
mask runs) are never treated as secrets. JSON awareness: skip keys ending in
`signature`, `id`/`ids`, path keys, objects with `type: image|base64`;
credential-context objects (host + user ⇒ `password` is secret); keyed splicing
preserving formatting; post-splice verification against alternate encodings
with structural re-encode fallback; duplicate-key lines re-encoded;
`ErrRedactionIncomplete` must fail the write. Byte-balanced ~1 MiB shards on an
errgroup (20 MB Codex transcript: 43 s → 2.1 s). `RedactedBytes` is a type-
level certificate only `JSONLBytes*` can mint. The local staging copy
`full.jsonl` is sanitized but **not** redacted (0600).

Images: no OCR, no vision scan. With `externalize_images` off, Claude stores
base64 inline unredacted (skip rule), Codex destroys them (entropy), Cursor
stores none. On, all land in `assets/` unredacted.

Compared with Atlas's redactor (`docs/atlas-reference/redact-README.md`): Atlas
has `sk-`/`ghp_`/`xox` prefixes, a count tally, a load-bearing negatives suite,
a pure no-I/O crate, redaction *before* local persistence, and the token
`[REDACTED]`. Entire has goredact, PII, OPF, rule packs, the JSON structure
awareness, the prefix cache and sharding, and the `RedactedBytes` contract.

### 5.2 Trust boundaries

| setting | honoured from | why |
|---|---|---|
| `redaction.openai_privacy_filter.command` | untracked local file, index+HEAD check | becomes argv[0] at push |
| `external_agents` | untracked local only | turns on executing `entire-agent-*` from `$PATH` |
| `allow_symlinked_agent_dirs` | untracked local only | a repo could ship `.claude -> /elsewhere` and vouch for it |
| review `prompt`/`task` | clone prefs or untracked local | go verbatim into bypass-permissions agents |
| `redaction.betterleaks/goredact` | **committed** file only | scanner choice must be team-visible |

A tracked `settings.local.json` is ignored entirely with a `git rm --cached`
hint. Hook commands always name the binary. `$PATH` scans use absolute entries
only (`filepath.Glob` bypasses `ErrDot`). All `.entire`, git-dir, worktree,
agent-config and session-store I/O goes through `os.Root` anchors from trusted
resolvers, refusing symlinked components; `.entire` itself must be a real
directory. Tokens: OS keyring by default, `ENTIRE_TOKEN_STORE=file` → 0600
JSON with flock; `ENTIRE_TOKEN` bypasses storage and derives the core origin
from the unverified `aud` claim (https, bare origin, no userinfo).

## 6. Command surface and settings

Groups: **Setup** `enable`, `disable`, `configure`, `status [--detailed|
--json]`, `agent {list,add,remove}`, `doctor {trace,logs,bundle,
migrate-checkpoints}`, `clean`, `plugin`. **Sessions & checkpoints** `session
{list,info,tokens,stop,current,attach,adopt,resume}`, `checkpoint {list,
explain,tokens,search,delete}`, `search`, `dispatch`, `activity`, `recap`.
**Account** `login`, `logout`, `auth`. **Control plane** `cluster`, `org`,
`project`, `repo`, `api`. Experimental (hidden on stable): `tokens`, `import`,
`review`, `blame`, `why`, `experts`, `runner`. Hidden: `mcp` (two read-only
tools: `agent_help`, `entire_status`), `hooks`, `trail`, `__send_analytics`,
`__sweep_sessions`. Plugins: `entire-<name>` on `$PATH` run as `entire <name>`
with a filtered environment. `agent-help` renders live machine-readable help
classified by audience.

`enable` flow: git-init offer when not a repo (local only, deferred initial
commit) → agent multi-select (detected built-ins pre-checked; external plugins
always discovered for the picker) → git identity preflight (login if needed)
→ checkpoint-remote picker when ambiguous → per-agent hook install (+ optional
skills) → `.entire/` + `.gitignore` → `settings.json` with `checkpoints.
primary = git-refs` → external agents flag to local file only → git hooks →
telemetry consent (default yes) → import-history offer (30 days, nothing
pre-checked) → "Ready." Re-runs route to agent management; `disable
--uninstall` restores `.pre-entire` backups and removes agent hooks.

`status` reads `ListReadOnly` and makes no network calls; `--json` exposes
`agents`, `active_sessions[]`, `hooks_outdated`, `codex_hooks`,
`checkpoint_sync_remote(_source)`, `unpushed_checkpoints`, `secret_scanners`.
`doctor` checks disconnected metadata, hook symlinks, missing/stale git hooks
(repairs), `.entire` symlinks, log sink, Codex trust, Antigravity tee, hook
drift, the retired deny rule (only auto-fix), retired Gemini hooks, summary
provider, legacy shadow branches (`--force` deletes), stuck sessions (owner
exited → auto-finalize; ACTIVE > 1 h; ENDED with pending work → condense/
discard/skip).

Settings: `.entire/settings.json` (strict, committed) ← clone prefs
(`<common-dir>/entire/preferences.json`) ← `.entire/settings.local.json`.
Keys: `enabled`, `log_level`, `telemetry` (opt-in, nil = ask),
`commit_linking` (prompt|always), `absolute_git_hook_path`,
`sign_checkpoint_commits`, `external_agents`, `allow_symlinked_agent_dirs`,
`summary_generation.{provider,model}`, `summary_timeout_seconds`,
`checkpoints.{primary,mirrors}`, `strategy_options.{push_sessions,
checkpoint_remote,checkpoint_push_remote,filtered_fetches,summarize}`,
`redaction.*`, `review_profiles`.

Resume: `session resume <branch>` checks out, walks first-parent to the
merge-base (≤ 100) for a trailer, reads the checkpoint (fetching on demand),
writes every session's transcript into **this** machine's agent store via
`WriteSession` (never overwriting without `-f`), then offers to launch the agent
or prints `claude -r <id>` / `codex resume <id>` / `copilot --resume` / `droid
--session-id` / `opencode -s` / `pi --session` / `agy --conversation` /
"Open this project in Cursor."

Import (`entire import <agent>`, experimental): 30-day lookback over the
agent's transcript directory, attribute to the repo by recorded `cwd`, split
turns per user prompt, anchor each turn to the last Claude-recorded commit SHA
that is an ancestor of the default branch (else the tip), deterministic ids
(`sha256(session/turn)[:6]` or a ULID from `createdAt`) so re-runs dedupe,
one checkpoint per turn with `kind: imported`, ended imported session state so
it lists but cannot resume. Importers exist for Claude, Codex, Copilot, Cursor,
Droid, Pi; none for OpenCode or Antigravity.

Telemetry: PostHog, opt-in, machine-id hash, event names `cli_command_executed`
(command path, flag *names*), `cli_plugin_executed`, `cli_skill_invoked`,
`cli_commit_condensed`, `cli_search_completed`; never prompts, paths or
queries; sent from a detached `__send_analytics` child;
`ENTIRE_TELEMETRY_OPTOUT` kills it.

## 7. Cloud, testing, release (briefly)

Control plane (`internal/coreapi`, generated from `core.openapi.json`): orgs,
projects, repos, mirrors, service accounts, clusters. Data plane per region
("cells") called by hand: checkpoints, `/me/{activity,sessions,recap}`, trails
(branch-centric work items with reviews, approvals, findings, SSE watch),
semantic search fanned out per cell. `git-remote-entire` is a separate binary
handling `entire://` URLs (smart-HTTP proxy with replica failover). Login:
browser loopback by default, device-code when no display/SSH/`--device`;
contexts in `contexts.json`, tokens in the keyring, auto-refresh.

Tests: unit (`-race`), integration (`HookRunner` runs `entire hooks <agent>
<verb>` as a subprocess with synthetic payloads and transcripts; no real agent),
E2E with real agents behind tmux (paid; not run proactively), a deterministic
canary agent **Vogon** that parses English prompts with regexes and fires hooks
(free; runs on every PR against both backends), `roger-roger` for the external
protocol, control-plane E2E against production with Playwright + TOTP. Lint:
golangci with ~60 linters, forbidigo bans `os.Getwd`, go-git `Reset/Checkout`,
`Worktree.Status`, cobra `Print*`; dupl threshold 75. A complexity tool
baselines LOC/cyclomatic/cognitive/coverage per command.

Release: goreleaser-pro, darwin/linux/windows × amd64/arm64, CGO off, signed
and notarized macOS (CI greps for the Developer ID and fails closed), Homebrew
casks `entire` and `entire@nightly`, Scoop, `install.sh` (sha256 verified,
`--channel nightly`), `install.ps1`; nightly tag at 06:00 UTC, nightly smoke
installs at 09:00; stable notes extracted from `CHANGELOG.md` (job fails if
missing); in-CLI version check every 24 h with an update prompt.

## 8. Known limitations (theirs)

`git commit --amend -m` can lose the trailer without a TTY; `git gc --auto` can
corrupt a worktree index via pruned loose objects (`gc.auto 0` advised);
concurrent ACTIVE sessions in one directory all condense on any commit
(spurious near-empty checkpoints; use worktrees); downgrading discards pending
work; the 7-day purge drops state even with pending work; a crash between
checkpoint write and state save on post-commit duplicates; mid-turn checkpoints
are never finalised if the session dies before Stop; `CheckpointTranscriptSize`
in the wrong coordinates silently stops a session condensing;
`CondenseAndMarkFullyCondensed` does not update it (inconsistency vs the two
other reset sites); `parseHookType` silently overwrites a malformed hook type
in the user's settings; the Claude settings rewrite reorders top-level keys;
Compaction's doc comment promises an offset reset the code does not do.

## 9. What this means for Atlas

### 9.1 What to take as-is

- The **hook model**: one event set, one dispatcher, per-agent
  `ParseHookEvent` only; the `sh -c 'command -v …; exec …'` wrapper; the
  "prefix `atlas hooks `" ownership rule and stale-entry pruning on every
  install; raw-map merges that preserve user hooks; 0600 atomic writes.
- The **turn algorithm**: untracked snapshot + transcript offset at turn start;
  transcript-from-offset + git-status diff at turn end; files identical to HEAD
  dropped; blob hashes for carry-forward.
- **Exit-code discipline**: silent 0 for everything that is not ours, 1 never
  2, stdout reserved for the banner and the one-time context injection.
- **Session-end on API error** (StopFailure), the flush-sentinel wait, the
  `.agent`/`.banner` O_EXCL claims, the 2 s lock bound at TurnStart.
- The per-agent facts in §3 (hook file, payload fields, transcript path and
  format, edit-tool names, token rules, quirks). These are the expensive part
  of this research; they transfer directly.
- Trust rules: hook commands name the binary; exec-bearing settings only from
  an untracked local file; `os.Root`-anchored I/O; absolute `$PATH` only.
- The Vogon idea: a free deterministic canary agent that exercises the whole
  hook path in CI.

### 9.2 What stays the Atlas way

- **Setup and identity**: `atlas init` binds org + cloud Project and writes
  `.atlas/`; `atlas agents add <agent>` installs the hook (Entire's `enable`
  picker flow is a good model for the interactive path).
- **Storage**: our capture store (SQLite, desktop byte-compatible) and cloud
  sync via `atlas serve`, not git refs. Session identity stays Atlas-minted with
  the agent's id in `native_session_id` (ADR-0001, kept). Checkpoints stay our
  model; `atlas capture link` replaces the trailer + post-commit condense.
  Whether to *also* stamp a commit trailer is open (it is what makes Entire's
  `blame`/`why`/`resume <branch>` work).
- **Shared memory**: `atlas_memory` over MCP, written into the agent's MCP
  config next to the hook (Entire has nothing here).
- **Redaction**: the desktop-compatible redactor, applied before local
  persistence (stricter than Entire).
- **Status/doctor/TUI** in our theme and command names.

### 9.3 Decisions required (the user's)

1. **Transcript files.** Entire's capture is hooks + reading the transcript
   file named in the hook payload. ADR-0001 / SPEC D4 and the ground rule forbid
   reading another agent's transcript files. Proposed replacement: *read only the
   file the agent's own hook payload points at, from the offset recorded at turn
   start, only while the hook runs; never scan agent directories for content.*
   The 30-day backfill (`entire import`) would be a separate, opt-in decision.
2. **Storage** stays Atlas store + cloud (not git refs), with an optional commit
   trailer to be decided.
3. **ACP, `atlas run`, the chat TUI, the registry installers and the ADR-0012
   import loop** are parked, the way the proxy was in Step 0.

### 9.4 Suggested order

1. ADR-0013 (hooks, transcript rule, parking ACP). Commit the ACP branch first.
2. Core: `atlas hooks <agent> <event>`, dispatcher, session state, turn-end
   extraction into the existing Recorder, redactor on everything, Vogon-style
   canary.
3. Claude Code end to end; then Codex, Cursor, Copilot, OpenCode (plugin),
   Droid, Antigravity, Pi.
4. `atlas agents add/remove/list`, `atlas status` hook health, `atlas doctor`,
   memory MCP entry written with the hook.
5. v0.2.0 for Linux and macOS.
