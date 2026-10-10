# Progress

One section per phase: what was built, what was verified live, what was not.

## Phase 0 — ADRs (done, accepted)

`docs/adr/0000`–`0007`, recording D7 and D13–D19.

## Phase 1 — scaffold (2026-10-10, awaiting review)

Built:

- Module `github.com/tryatlas/atlas-cli`, `go 1.23.0`; one dependency so far
  (`spf13/cobra`). Later phases add bubbletea v2, modernc sqlite and blake3 as
  they need them.
- `cmd/atlas`: the full command tree from SPEC §1.1, §3.7, §4.4, §5.5, §6.7,
  §7.4–7.6, §8 and D13 (`memory verify|export|import`). Every command is a stub
  that prints `atlas <path>: not implemented` to stderr and exits 2; usage
  errors exit 1. `hook`, `statusline` and `mcp-bridge` are hidden (agents call
  them, people don't). Version comes from `-ldflags -X main.version`.
- `internal/…`: one package per SPEC §2 line, each only a `doc.go`.
- `Makefile`: `build test lint fmt tidy clean`, with `CGO_ENABLED=0` exported.
- `.golangci.yml` (v2 format): standard linters plus bodyclose, errorlint,
  gosec, misspell (UK), nolintlint, revive, unconvert, unparam; gofmt and
  goimports with the module as the local prefix.
- `.github/workflows/ci.yml`: build and test on native runners for
  linux/amd64 (`ubuntu-24.04`), linux/arm64 (`ubuntu-24.04-arm`) and
  darwin/arm64 (`macos-15`) with Go from `go.mod`; a lint job with
  golangci-lint v2.14.0 and a `go mod tidy -diff` check.

Verified locally (darwin/amd64, Go 1.27.2):

- `make fmt lint test build`: 0 lint issues; tests pass. The tests check the
  exact command surface, the stub message and exit code for every command,
  a usage error, and `--version`.
- `GOOS/GOARCH` cross-builds for all three CI targets succeed with
  `CGO_ENABLED=0`.

Not verified:

- The CI workflow has not run: there is no GitHub remote yet. Runner labels and
  action versions are current as far as I know but untested here.
- Tests ran under Go 1.27.2, not the 1.23 floor that CI will install.
- Not run on any arm64 machine; only cross-compiled.

## Phase 2 — internal/redact (2026-10-10, awaiting review)

Built:

- `redact.Flat` (the desktop's `redact`) and `redact.Auto` (`redact_auto`,
  JSON and JSONL aware), returning the text plus per-category `Counts` that
  marshal as `{"<category>": n}`. A panic inside becomes `*redact.Error`.
- Six layers: entropy, 305 vendored betterleaks rules, 37 provider prefixes,
  credentialed URIs, connection strings, credential key/value pairs. Parameters
  and the reasons for them are in ADR-0008.
- `tools/genrules`: regenerates `internal/redact/vendor_rules.json` from a
  betterleaks config, recording every excluded rule with its reason.
  `LICENSE-betterleaks` sits beside the table.
- Speed: one Aho–Corasick pass picks the vendor rules to run, and per-line
  prefilters gate the other layers. Large texts run the active rules in
  parallel.

Tests (`go test ./internal/redact`): a fixture per layer with exact output and
counts; overlap counting; idempotence of both passes; 47 hold-outs
(placeholders, `${VAR}`, mask runs, SHAs and digests, UUIDs, identifiers,
paths, type hints, code, prose), 15 of which are regressions found by scanning
this repo's docs; the JSON walk (key order, number literals, escaping, id,
path, URL and credential keys, arrays); JSONL; fallback to flat; counts JSON;
table integrity; panic → error; the keyword matcher.

Verified locally: scanning every text file in this repo (SPEC, evidence, ADRs,
source) redacts exactly one line, the README's own `API_KEY=…` example.
Throughput on Intel i5: 47 MB/s on ordinary code, 2.7 MB/s on prose dense with
rule keywords.

Not verified / differs:

- **Not parity with the desktop.** The desktop's `vendor_rules.rs`, `lib.rs`
  and test corpus were not available. Rule counts and category tallies can
  differ (ADR-0008). Upstream betterleaks has moved since the desktop
  generated its table (466 rules today, ~310 then).
- Go's `\w`, `\b` and `\s` are ASCII-only, while Rust's regex crate is
  Unicode-aware by default. Vendored rules can therefore differ on non-ASCII
  text.
- The JSON walk keeps key order. serde_json without `preserve_order` sorts
  keys. Which one the desktop uses is unknown (`atlas-redact/src/lib.rs:286-349`
  would say).

## Phase 3 — internal/auth (2026-10-10, live acceptance pending)

Built:

- `internal/hosts`: base URL ladders (env, then build-time `-ldflags -X`, then
  default), normalised. There is no on-disk override.
- `internal/timefmt`: chrono `to_rfc3339` format (`+00:00`; fractional seconds
  only when non-zero, in 3, 6 or 9 digits). Capture will reuse it.
- `internal/auth`:
  - **session file**: per-OS path, serde-pretty layout written byte for byte,
    camelCase fields in the desktop's order, `orgs` null and `[]` kept
    distinct, `pinnedOrg {"orgId":null}` kept, unknown roles read as null,
    fields from a newer desktop preserved, atomic `atlas-session.tmp` (0600)
    then rename then chmod 0600, a corrupt file reads as signed out;
  - **client**: the desktop's status classification, Retry-After as
    delta-seconds, every endpoint the CLI uses, error text that never carries
    a URL, header or body;
  - **device grant**: 200 ms poll floor, slow_down adds 5 s cumulatively,
    keeps polling through 5xx, a local deadline, and cancel sends nothing;
  - **identity refresh**: get-session, organisation list, then roles from the
    JWT claim. Failures keep what is stored, and only a 401 from `/token`
    clears the session. Re-reading the file after the network calls means a
    concurrent sign-out wins. The pin is kept;
  - **logout**: delete the file first, then one `/sign-out`;
  - **JWT `TokenSource`**: cached in memory until exp − 60 s (600 s fallback),
    with `Invalidate` for a 401;
  - **backoff**: 1 s → 300 s, jitter in [cap/2, cap], Retry-After clamped to
    [1, 300] s.
- Commands: `atlas login [--no-browser]`, `logout`, `whoami [--refresh]`,
  `org list|use <id|none>|create <name> [--slug]`. Login prints the plain
  `verification_uri` and the code, and opens a browser only on a local desktop
  session.

Tests: the session file (SPEC example round-trips byte for byte, fresh-grant
shape, minimal files, null vs [], explicit-none pin, unknown role, unknown
fields, no HTML escaping, mode 0600, no temp file left, corrupt → signed out,
active-org resolution, per-OS dir), the grant against a scripted server (exact
bodies, poll classification table, intervals, terminal outcomes, deadline,
cancel), refresh rules, 401-only clearing, no resurrecting a deleted file, pin
across refresh, logout order, classification, the JWT cache, backoff, and the
CLI commands end to end against a fake host. Command tests can no longer reach
the real session file or auth host: `TestMain` installs a refusing default.

Incident: while wiring the commands, the old stub test ran the real
`atlas login`. It started a device grant against `auth.tryatlas.cc` and polled
until stopped. No token was issued and no file was written; there was no
session file before or after. Fixed by the hermetic default above and a 5 min
test timeout.

Not verified:

- **Live acceptance** (`atlas login` against the real host, then the macOS
  desktop app reading the file unchanged) needs the user to approve the code
  in a browser. Not yet run.
- The desktop downloads the avatar and writes `avatarPath`. The CLI does not
  fetch it: it keeps a stored path while the URL is unchanged and writes null
  otherwise. The desktop fetches it when it next refreshes.
- Chat and web URLs: the desktop uses a blank env value as is. The CLI falls
  through to the default for every host.

## Phase 4 — internal/project and `atlas init` (2026-10-10, awaiting review)

Built:

- `internal/capture/store` (first part):
  - opens `.atlas/sessions.db` with the desktop's pragmas (WAL, NORMAL,
    foreign keys, 5 s busy) and refuses a version above 10 or between 1 and 9;
  - creates the v10 schema from the desktop's **stored DDL**, so
    `sqlite_master` matches a desktop-created store byte for byte (comments
    and ALTER-added columns included);
  - writes `.atlas/.gitignore` with the desktop's exact text;
  - **writer lock**: `sessions.lock`, `BEGIN EXCLUSIVE`, `busy_timeout(0)`;
  - **binding row**: upsert keeps `created_at`. `BindCloud` promotes `local`
    rows to `pending` (local → cloud) and requeues every row when switching
    cloud Projects.
- `internal/project`:
  - canonical paths, and the scope root (git common dir, bare repos, outside
    git);
  - walk-up discovery to the nearest binding, stopping at the scope root
    (ADR-0007);
  - git fingerprint (root commit, shallow flag, normalised origin URL);
  - the memory gate file (absent or unparsable means on).
- `internal/ingest`: workspaces client (list, slug-available, create,
  connect); refreshes the JWT once on a 401; absent fields are omitted.
- `internal/tui`: a Bubble Tea v2 single-choice picker. Commands fall back to
  flags when stdin/stdout are not terminals.
- Commands:
  - `atlas init [--org] [--project id | --create [--slug --name --visibility]] [--local] [--yes]`.
    It takes the writer lock and refuses with the desktop's own message when a
    desktop window is recording. It asks the server to match by fingerprint,
    then offers the org's Projects or "create". Re-binding needs confirmation,
    and so does a subfolder (with ADR-0007's warning);
  - `atlas memory enable|disable` and `atlas capture enable|disable` (gates
    only);
  - `atlas status` (account, Project, binding, both gates, recorder).

Tests: schema against the desktop oracle (`testdata/desktop_sqlite_master_v10.json`,
extracted from this repo's desktop-written store, schema only), columns against
the evidence, version guards, the lock, binding upsert, promote and requeue,
scope roots with real git (subdirectory, worktree, bare, outside git),
discovery (nested, store without a binding, stopping at the scope root), URL
normalisation, the gate file, the ingest client, the picker, and the commands
end to end (local; cloud matched by fingerprint; create; no match without a
TTY; subfolder needs --yes; refusal while locked; gates and status from a
subdirectory).

Verified live:

- `TestLiveDesktopLock`-style check: `store.Lock` on this repo, whose desktop
  window is recording, got `ErrLocked`. The lock interoperates with the
  desktop's.
- In `~/Codes/atlas-test-2` (which the desktop app also has open):
  `atlas init --local`, `status`, `memory disable|enable` and
  `capture disable|enable` all worked. sqlite3 reads `user_version = 10` and
  the binding row.

Differs from SPEC (evidence wins):

- SPEC §4.4 step 4 sets `import_approved=1` for a cloud binding.
  `evidence-capture.md:641-644` says registering a cloud Project clears it,
  and the desktop imports transcripts only after the user confirms. Cloud
  init writes 0; `--local` writes 1, as the desktop's local enable does.

Not verified:

- A **cloud** init against the real ingest host. It would create or connect a
  real Project in the user's organisation; not run without asking.
- Whether the desktop shows the CLI-bound folder's capture as on (local) in its
  capture popover.
- `go.mod` now says `go 1.26.0`, because modernc.org/sqlite v1.60 requires
  it. That still satisfies "Go 1.23+", but CI's Go version moves up with it.

## Phase 5 — shared memory (2026-10-10, awaiting review)

Built:

- `internal/memory/record` (`memory.sqlite` v6):
  - creates the schema from the desktop's stored DDL (sqlite_master matches
    byte for byte) and refuses a version other than 6;
  - `Clean` + flat redaction on every stored text (key, content, event
    payload strings, feedback notes, import notes);
  - content hash, and identity by (kind, key) else (kind, hash);
  - merge, replace, the 5-minute retry rule, and the guard with the exact
    conflict text;
  - every write appends a revision sealed into the blake3 chain, sets
    `entries.rev`, and rewrites the FTS row in the same transaction;
  - the tool path logs an event for an insert or replace;
  - forget (tombstone, `retracted_events`, `forgotten`), feedback (useful,
    promote, wrong, stale), archive;
  - session start and end; event folds (plan, file_changed and the durable
    kinds) ready for capture;
  - FTS5 search with the query builder and rank fusion with the trust and
    recency priors; changes paging; history; writer URIs; contradicts links;
  - citations (parse, path rules, hash, re-check as valid, moved, stale or
    unverifiable);
  - `VerifyChain`, and Agent Memory Repo export and import (imports land as
    candidates, `import:amr`, metadata kept in the revision note, deduped).
- `internal/memory/server`: an MCP JSON-RPC core with the ten tools. Their
  descriptions and the instructions are generated from SPEC §5.7 and checked
  identical to `evidence-memory.md` §3.5. Also:
  - `tools/list` with `ttlMs` and `cacheScope`;
  - the sharing gate on every call;
  - the entry JSON shape;
  - briefing ranking and caps; changes clocks per session;
  - `memory_why` and provenance from the capture store, read-only;
  - `project.json` on the first write.
- Transports:
  - streamable HTTP (`Mcp-Session-Id`; 404 for an unknown session; DELETE
    ends it);
  - bearer tokens in memory (ACP path);
  - a Unix socket in a 0700 directory, mode 0600, with a peer-uid check
    (`LOCAL_PEERCRED` on darwin, `SO_PEERCRED` on Linux; pure Go via
    x/sys/unix).
- `atlas mcp-bridge`: either the desktop form (`<url>` with
  `ATLAS_MCP_TOKEN`, loopback only, no proxy) or the ambient form
  (`--project --agent`). The ambient form uses the listener's socket from
  `.atlas/cli-run.json`, else serves memory in-process. Each process mints a
  UUID session id. It writes one message per line, replays `initialize` on a
  non-JSON-RPC 404, and answers with a JSON-RPC error when the server is
  unreachable.
- `internal/listener` + `atlas serve` (foreground; `--daemon`/`--stop` come
  with the proxy), and `atlas memory verify|export <dir>|import`.

Verified against the desktop:

- **Chain encoding**: recomputed every link of two desktop-written records
  (46 and 37 revisions; ops fold and insert); all match byte for byte
  (`TestOracleChains`, run with `ATLAS_ORACLE_MEMORY=…`; the fixtures hold
  real memories and are not committed).
- **Content hash**: reproduced all 83 desktop `content_hash` values.
- **Round trip** (Phase 5 acceptance, `TestOracleRoundTrip`): opening copies
  of both records and running every read adds no revision and the chain
  verifies. A CLI write then extends the desktop chain and it still verifies.
- **Schema**: the CLI's `sqlite_master` equals three desktop stores (two with
  data, one fresh).
- **Formats from the data**: `session_start` payload `{}`; `file_changed`
  payload `{"path","summary"}` with key = path and content = summary; the
  event `ts`, entry `updated_at` and revision `at` are the same; `project.json`
  is compact with the 12-hex id of sha256(path).
- **Live**: `atlas memory verify` on this repo's record (56 revisions, being
  written by the desktop during this session) and on `~/Codes/atlas` (37):
  intact. In `~/Codes/atlas-test-2` (a desktop-created record), a remember
  and a briefing through the in-process bridge, then a search from a second
  agent through `atlas serve`'s socket; the chain is intact, and the events
  sit beside the desktop's own `session_start`.

Unclear in the reference; chosen, not verified:

- **Citation hash and JSON keys.** No desktop record on this machine has a
  cited memory, so "blake3 of the cited lines with whitespace collapsed per
  line" was implemented as lines joined with `\n`, hex-encoded, stored as
  `{"path","start_line","end_line","symbol"?,"hash"}`. If the desktop joins
  or encodes differently, it will mark CLI-written citations stale.
- **Keyed write onto a keyless entry.** SPEC §5.3 says identity is
  (kind, key) when a key is given, and also that "a keyless survivor adopts
  the key". That only happens if a keyed write falls back to a keyless entry
  with the same hash, so the CLI does that.
- **Merge revisions** name the writer of the merge (who restated it), not
  the entry's first writer. No desktop merge revision was available to check.
- **`ttlMs` / `cacheScope`** are placed at the `tools/list` result level.
- **`memory_changes` paging**: each kind takes its 8 oldest changes since the
  clock; the clock then stops before the first one left out. The page is
  shown newest first.
- **Rank fusion constant** k = 60. Index lines are cut to 160 characters
  with "…".
- **Agent ids**: the desktop's data names Claude over ACP `claude-acp`;
  SPEC §5.1 says `claude-code`, which the CLI's ambient adapter will use.

Not built yet: agent MCP config writing (`atlas memory enable` writes the
gate only; the adapters come in Phase 6), the curated pack and recent-session
extras (omitted as SPEC allows), handoff notes written by the CLI (it serves
one when an episode exists), and commit evidence.

## Phase 6 — session capture and the Claude Code adapter (2026-10-10, desktop check pending)

Built:

- `internal/capture/store` rows:
  - session upsert (model and cwd: new value wins; branch: first wins),
    turns, and `SetTitleIfAbsent`;
  - messages: once per native id, 2 KiB preview, content hash, body bytes,
    blobs above 64 KiB (`blobs/<hh>/<sha256>`, tmp + fsync + rename);
  - tool-call upserts with the COALESCE and `'[]'` rules, binary results as
    blobs; file touches; agent edits;
  - the cumulative usage recorder (per-field deltas against `usage_cursor`);
  - ordered `redaction_counts` merges, the needs-attention flag, and
    `sent → pending` resync.
- `internal/capture/record`: machinery stripping, titles, canonical tool
  names and ACP kinds, write paths, edit patches, project paths, sketches.
  The Recorder (single writer) handles:
  - the prompt (turn, title from the flat redaction, user message, git
    anchor) and assistant runs;
  - tool calls: earlier sightings fill in later ones; on the first completed
    sighting, the written files are sampled with `existed_before` from HEAD,
    and a failed call records no writes. Shell calls are diffed against the
    anchor, and files a file tool wrote are added to the anchor;
  - usage as a running sum seeded from the cursor (ADR-0005); turn end;
    redaction failure → needs_attention.
- `internal/proxy`:
  - bytes forwarded untouched with `FlushInterval -1`; headers are passed
    through and never logged; the upstream is the user's previous
    `ANTHROPIC_BASE_URL`, else the API;
  - copies are parsed after forwarding through one FIFO queue: the request
    half when it arrives (tool results, file sampling, prompt) and the
    response half when it ends (runs, tool uses, usage, turn end);
  - the Anthropic dialect, SSE and JSON, gzip and deflate decoded for the
    parser; session id from `metadata.user_id`'s `session_<uuid>`, else the
    only hook-registered session, else a hash;
  - only the main conversation is recorded: helper requests without tools
    and sidechains are skipped; a compaction restarts it;
  - `ATLAS_PROXY_TRACE=<file>` writes exchange shapes (roles, block types,
    counts; no text, no headers) for diagnosis.
- `internal/listener`: the loopback proxy port and `/hook` on the
  peer-checked socket; `cli-run.json` gains the port.
- `internal/agents/ambient`:
  - an order-preserving JSON editor and `.atlas/cli.json` state;
  - the Claude Code adapter: MCP entry in `.mcp.json` (no secret) approved
    in `.claude/settings.local.json`; four command hooks and the status line
    (wrapping an existing one) in `.claude/settings.json`; removal restores
    the user's files byte for byte.
- Commands:
  - `atlas shell`: writer lock, listener, env (`ANTHROPIC_BASE_URL`,
    `ATLAS_PROXY_PORT`, `ATLAS_SHELL`, `NO_PROXY`), `$SHELL -i`;
  - `atlas env [--shell sh|fish]`;
  - `atlas hook claude session-start|user-prompt-submit|stop|session-end`:
    never fails; the session-start banner is JSON only, `systemMessage`, no
    `additionalContext`;
  - `atlas statusline claude`;
  - `memory enable|disable` and `capture enable|disable` now install and
    remove the Claude Code entries.

Verified against the desktop's own data (this repo's desktop-written
sessions.db, profiled from a copy):

- `sketch_after`: reproduced on all 257 file touches whose files are
  unchanged. FNV-1a 32 over distinct trimmed non-blank lines, sorted
  numerically, unpadded lowercase hex.
- Message `content_hash` = sha256(body), preview = the first 2 KiB,
  `body_bytes` = byte length: 131 of 131 rows.
- Title derivation reproduces the desktop's stored title for this session
  (119 characters + "…").
- Formats taken from the data: `token_totals` key order, `redaction_counts`
  in insertion order, lowercase ACP kinds, `locations` as `[{"path",…}]`, and
  the `--- before / +++ after` patch.

Verified live (Claude Code 2.1.296, headless `claude -p` through
`atlas shell` in `~/Codes/atlas-test-3`, local binding):

- One session under Claude's own session id. Source `acp`, agent
  `claude-code`, model, branch, and title from the prompt.
- One completed turn; the user prompt and the assistant reply.
- Write, Edit and `mcp__atlas__memory_search` tool calls with results and
  locations. The file touches carry the Write-time and Edit-time versions of
  `notes.md`, and the patches match the edits.
- Usage recorded for the turn; `redaction_counts` empty.
- Memory through the bridge in the same run: remember, then search.
  `memory verify` intact; a memory citing a deleted file came back stale.

Found and fixed by the live runs:

- Claude Code 2.1 puts a `system`-role message after the user's prompt, so
  "last message is a user text" missed every prompt. A turn now starts from
  the newest user text among the messages added since the last request.
  Regression test added.
- Sampling at the end of the response raced the agent's next tool, so a
  Write was recorded with the file as the following Edit left it. The
  request half now runs when the request arrives.
- The entropy layer redacted `atlas-session:claude-code/<uuid>` writer URIs
  in MCP results. Embedded UUIDs are now held out. Regression test added.

Not verified / known gaps:

- **Desktop acceptance**: open `~/Codes/atlas-test-3` in the desktop and
  check the Timeline (title, tool calls, file touches) and that the session
  appears once. The local binding has `import_approved=1`, so the desktop's
  importer also reads Claude's transcripts. Earlier test runs (whose CLI rows
  were reset) may therefore appear as imported sessions; the captured one
  should not be duplicated, because its native id is Claude's session id.
  I did not look in `~/.claude` to confirm the ids match (ground rule).
- `metadata.user_id` carries `session_<uuid>` in Claude Code 2.1.296
  (seen live: "user_id_has_session=true"). Hooks were installed but not
  separately confirmed to fire in `-p` mode.
- The memory bridge mints its own session id; it is not yet joined to the
  capture session (ADR-0003's "needs a live check").
- Brotli responses would be forwarded but not parsed (none seen live).
- Git linkage (§6.6) is not built; the desktop links commits on open.
- The adapter is a concrete type; the shared `Adapter` interface
  (ADR-0000) will be extracted with the Gemini and Codex adapters in Phase 7.

## Phase 6b — closing the Phase 6 gaps (2026-10-11, desktop checklist pending)

Built:

- **Memory and capture share a session id** (ADR-0003's open check):
  - `internal/procs` reads process ancestry (`/proc` on Linux, `ps` on
    macOS);
  - hooks send their ancestor pids to the listener, and the bridge sends its
    own over the peer-checked socket (`X-Atlas-PIDs`);
  - the listener joins a bridge to a hook session when their nearest common
    ancestor is the agent process, never the listener, its ancestors or the
    shell it started; failing that, the only session in the same cwd started
    within two minutes;
  - the MCP session is then renamed to Claude's session id, with its clock
    moved.
- **Hook field trace**: `ATLAS_HOOK_TRACE=<file>` records each hook's field
  names, never values.
- **`Adapter` interface** (`internal/agents/ambient/adapter.go`). It follows
  SPEC §7.1, with `Shell(Listener) (ShellSetup{Env, Shims})` replacing
  `ProxyEnv` (ADR-0004) and `Configured()` added. The Claude adapter is
  registered behind it. `atlas shell`, `env` and `memory/capture enable`
  iterate adapters; shims go to a private temp directory prepended to PATH
  in the shell only.
- **Git linkage, watcher-free** (`internal/capture/git`):
  - runs on `atlas shell` exit and on a new `atlas capture link`;
  - the walk: cursor → `rev-list --first-parent --reverse` (max 200 without
    a usable cursor; `recovered=1` when the cursor commit is gone); no
    cursor and no candidates sets the cursor to HEAD; nothing happens mid
    rebase, cherry-pick or merge;
  - a merge links its side commits; the asymmetric rule (deleted ↔ deleted;
    existed-in-parent and existed_before → path alone; else blob sha, the
    `--filters` sha, or sketch containment ≥ 0.5); a session must have
    started by the commit time; paths match case-insensitively and through
    renames;
  - checkpoint upsert keyed (session_id, commit_sha) with patch-id, branch,
    author, files and numstat; touches consumed with the +1 s bound; cursor
    advanced per commit.
  - **Not built:** rewrite reconciliation (relink by patch-id after a
    rebase, orphaning, recovering orphans). The desktop does it when it
    opens the folder.
- **Brotli**: the parser's copy is decoded (andybalholm/brotli, pure Go);
  forwarding was already untouched.
- `docs/acceptance/desktop.md`: the one-page desktop checklist.

Verified live (Claude Code 2.1.296, `~/Codes/atlas-test-3`):

- **Hooks fire in both modes**, all four events. Fields received:
  - SessionStart: `cwd, hook_event_name, session_id, source,
    transcript_path`, plus `model` in interactive mode;
  - UserPromptSubmit: `cwd, hook_event_name, permission_mode, prompt,
    prompt_id, session_id, transcript_path`;
  - Stop: `background_tasks, cwd, effort, hook_event_name,
    last_assistant_message, permission_mode, prompt_id, session_crons,
    session_id, stop_hook_active, transcript_path`;
  - SessionEnd: `cwd, hook_event_name, prompt_id, reason, session_id,
    transcript_path`.
  `transcript_path` is never read (ADR-0001). The interactive run went
  through `expect`; I accepted Claude's folder-trust prompt for this scratch
  folder.
- **Join**: the decision Claude stored in a `-p` run carries session
  `5d908253…`, the captured session's native id. `memory_why notes.md` names
  both captured sessions with their titles and lists that decision under the
  session that wrote it.
- **Git linkage**: committing `notes.md` and running `atlas capture link`
  wrote two checkpoints for commit `4b3c934` (two sessions wrote it,
  matching the desktop's "two Sessions → two Checkpoints"); all three
  touches consumed; cursor at the commit.

Not verified / open:

- The desktop checklist (Timeline, duplicates, Usage totals, checkpoints,
  Memory Health) needs the user.
- Checkpoint `insertions`/`deletions` are the whole commit's numstat. The
  evidence does not say whether the desktop counts only the linked files.
- The `hook codex notify` stub will be removed (ADR-0004: no notify hook in
  v1).

## Phase 7a — cloud sync

Built:

- **Outbox** (`internal/capture/store/outbox.go`): pending rows in drain
  order (sessions by `started_at`, messages and tool calls by `seq`,
  checkpoints by `created_at`; rows of aborted turns never leave);
  `RetireFailed` (`sync_attempts >= 5` → `failed` at the start of a drain);
  guarded `MarkSent` (a row whose hashed content changed during the upload
  stays pending); `BumpAttempts`, `MarkFailed`, `RetryFailed`,
  `SetDrainState`, `OutboxCounts`, `ReadBlob` (a hash mismatch counts as
  missing).
- **Drain** (`internal/capture/sync`): the four artifact types with the
  exact key order and the four `contentHash` formulas (null columns format
  as the empty string); blobs `PUT` before the batch that references them
  (2xx/409 ok, 413 or missing locally fails the row); batches ≤100
  artifacts and ≤4 MiB; 401 → refresh once per pass; 403 →
  `drain_state='not_authorized'` and no further calls; 429 → Retry-After;
  5xx or no answer → offline; another 4xx bisects to the poison row, which
  fails; `"accepted":false` counts an attempt; a row absent from a non-empty
  `results` list is treated as accepted, like an empty body. The pass stops
  starting work after 3 s.
- **Runner**: drains at start, every 30 s once the backoff allows (30 s
  doubling to 15 min, `max(Retry-After, backoff)`), and on demand.
- `atlas shell` and `atlas serve` share one host (`cmd/atlas/host.go`):
  writer lock, recorder + proxy, memory, drain. On exit: git link, then
  one last drain pass.
- `atlas serve` now runs capture too; `--daemon` re-runs it detached
  (`Setsid`, log in `.atlas/cli-serve.log`) and waits for its run file;
  `--stop` signals only a daemon's listener (the run file records
  `daemon: true`).
- `atlas capture sync` and `atlas capture retry`: in-process when the store
  is free, else `POST /sync` on the running listener's socket. A human sync
  clears `not_authorized` and tries again.

Verified live (`~/Codes/atlas-test-cloud`, created in "Atlas org" as cloud
Project `atlas-cli-acceptance`, id `01M4KNA93EHF0J98GEC08F6JZ2`):

- `atlas serve --daemon`, `eval "$(atlas env)"`, `claude -p` wrote
  `notes.md`; `atlas capture sync` → the listener drained: session, two
  messages and one tool call `sent`, no attempts used.
- Commit `66a94e4`, `atlas serve --stop` → linked on exit and the
  checkpoint `sent`; `drain_state` stays `ok`.

Not verified: that the session shows in the cloud Workspace and in a
teammate's Timeline. That needs the web app or a second account (the user).
Open: the evidence does not say how the desktop treats a row that is
missing from a non-empty `results` list; the CLI counts it as accepted.
