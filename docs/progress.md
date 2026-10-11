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

## Step 0 (ADR-0009) — commit and park

- Committed Phases 6b and 7a as `6e8a73f` on `feat/cli-foundation`
  (graphify-out/ ignored); branch `parked/proxy-path` points at it.
- Removed from `feat/cli-foundation`: `internal/proxy`,
  `internal/agents/ambient`, `internal/procs`; the `atlas shell`, `atlas
  env`, `atlas hook …` and `atlas statusline` commands; the hook, status
  line and MCP-config writers behind `memory enable` / `capture enable`
  (they now flip only the gates and write no agent file); the memory
  server's socket auth (`SocketAuth`, the `X-Atlas-*` grant headers), the
  bridge-to-hook PID join and `Server.Rename`; the `andybalholm/brotli`
  dependency.
- `atlas mcp-bridge <url>` is now only the desktop's bridge (ADR-0019): a
  loopback URL with the bearer in `ATLAS_MCP_TOKEN`.
- The listener serves `/mcp` on `127.0.0.1:<port>` behind per-session
  bearers (`Listener.Tokens`, `MCPURL()`), and keeps the owner-only Unix
  socket with the peer-uid check **for one path only, `/sync`**: `atlas
  capture sync|retry` use it to drive the drain of a running listener. The
  socket's `/mcp` and `/hook` are gone.
- `atlas serve` (and `--daemon/--stop`) runs the listener with the store
  lock and the drain; no proxy.
- `docs/acceptance/desktop.md` is marked as belonging to the parked path.

Tests and lint green. Command surface now: login, logout, whoami, org
list|use|create, init, memory enable|disable|verify|export|import, capture
enable|disable|sync|retry|link, serve, status, run (stub), mcp-bridge
(hidden).

## Phase 7 (first part) — the ACP client, up to the first end-to-end agent

ADR-0010: the JSON-RPC layer is hand-rolled; `coder/acp-go-sdk` v0.13.5 was
checked (zero deps, Apache-2.0, every method we use exists) and not taken.
The reasons: its unions are strict where the desktop is tolerant, it closes
the connection on lines over 10 MiB and on a notification backlog over 1024,
and it logs raw message lines.

Built (`internal/agents/acp`, `internal/run`, `cmd/atlas`):

- **Registry**: cache-first at `$XDG_DATA_HOME/dev.atlas.ide/external-agents/registry/registry.json`,
  hourly refresh, a failed refresh keeps the catalogue, entries parsed one
  at a time (bad ones dropped), platform keys, the binary-vs-npx rule, icon
  URLs, User-Agent `atlas/<version>`.
- **Installed map**: `external-agents/installed.json` in Zed's shape (two-space
  indent, `type` first, empty options omitted, unknown keys round-trip,
  `extension` read as `registry`, `~/` expanded at launch only).
- **Managed Node v24.11.0**: SHASUMS256 verification required, liveness via
  `npm --version`, the exact npm flags, `npm_config_*`/`NODE_ENV` stripped,
  1800 s / 600 s deadlines with one retry on spawn failure only.
- **npx installs**: ceiling spec `pkg@0.0.0 - <ver>`, `.atlas-wanted`, the five
  install-needed checks (platform optional package verified against
  `node_modules/.package-lock.json`), `--prefer-online` then
  `--prefer-offline`, bin resolution.
- **Binary installs**: the versioned-dir formula, checksum ladder (registry
  sha256 → GitHub release digest → unverified), accepted/refused suffixes,
  raw-binary fallback, 2 GiB / 4 GiB / 100 000-entry limits, files 0755,
  `cmd` containment, old versions removed.
- **Launch**: piped stdio, `setsid`, `killpg(SIGKILL)` on teardown,
  `CLAUDECODE` stripped, proxy variables canonicalised with `NO_PROXY`, the
  desktop's quirk table as data, env precedence process < distribution <
  extra < (BYOK) < installed-map env.
- **JSON-RPC**: one serial queue for notifications and responses in wire
  order, agent requests on their own goroutines, `$/cancel_request` both
  ways, no line limit, nothing logged.
- **Protocol**: `initialize` exactly as the desktop sends it, 60 s timeout,
  250 ms exit grace, 1 s stderr drain, protocolVersion ≥ 1; 120 s for
  session/new|load|resume|authenticate|set_mode|list; no timeout on
  prompt; cancel with 5 s grace and the abort-string rule; the
  authenticate outcome rules; session/new with the `atlas_memory` entry
  (HTTP + bearer when `mcpCapabilities.http`, else the stdio bridge with
  `ATLAS_MCP_TOKEN`) and `_meta.claudeCode.options.allowedTools`;
  `additionalDirectories` only when advertised; modes synthesised from a
  `category:"mode"` select; `default_mode` applied; load/resume, set_mode,
  set_config_option, list, delete, close, logout by capability.
- **Client callbacks**:
  - `fs/read_text_file` and `fs/write_text_file`, with root containment that
    canonicalises the deepest existing ancestor;
  - `terminal/create` (asks first, drops `DYLD_*`/`LD_*`, own process group,
    UUID ids), plus `output`, `wait_for_exit`, `kill` and `release`;
  - `session/request_permission` (the option kind is read back from the
    request; a cancelled turn answers cancelled);
  - elicitation in `form` and `url` modes.
  Unknown `sessionUpdate` variants and unknown session ids are dropped.
- **Recorder for ACP**:
  - one message per contiguous run of one kind, written at turn end with
    the first chunk's time;
  - tool sightings merged by call id;
  - `diff` content blocks drive write sampling and the patch;
  - `existed_before` comes from the first non-terminal sighting;
  - the 60 s shell window;
  - prompt `usage` per turn, and `usage_update` as gauges;
  - nothing recorded during a `session/load` replay.
  `source=acp`, `native_session_id` = the ACP id, `agent` = the registry id.
- **Memory**:
  - `session_start` at bind and `session_end` at close (no longer on an MCP
    DELETE);
  - `plan` → `plan_set`; completed write tools → `file_changed`;
  - the token is minted before `session/new` and bound to the id after it;
    the HTTP handler now reads the grant from the bearer on every request.
- **Commands**: `atlas agents list|install [--detected]|uninstall [--purge]|detect|logout`,
  `atlas run <agent> --prompt "…" [--no-tui] [--yes] [--resume <id>]`.
  Headless permissions: `--yes` allows; on a terminal the user picks;
  otherwise the first reject option.
- `atlas serve` and `atlas run` share `internal/run.Host`. When the desktop
  holds the lock, `atlas run` runs with capture off and says so.
- Fixture tests: a scripted ACP agent (`acptest`, the test binary re-run as a
  child) covers initialize, session/new/load, prompt, permission (allow and
  reject), fs/write_text_file, plan, usage_update, an unknown variant, and
  AuthRequired → terminal login → authenticate "not implemented" → retry.

Verified live, `claude-acp` 0.89.1 (npx), `~/Codes/atlas-test-cloud`:

- First run downloaded Node v24.11.0 (checksum verified) and installed the
  package, then wrote `acp-notes.md` with **Write**: session, prompt and
  reply, tool call, file touch (`existed_before=0`, hash, sketch), patch,
  usage per turn plus context gauges (31 370 / 1 000 000), turn completed;
  every row `sent` to the cloud Project.
- A **Bash** edit was captured as a call. It has no file touch, because the
  file was already untracked before the command ran; the desktop's rule
  gives the same.
- An **Edit**, then the agent's own `atlas_memory` `memory_remember`, ran with
  no permission prompt, so `allowedTools` worked. Memory holds
  `file_changed acp-notes.md` and the decision under the ACP session id.
- Commit `a38e248` + `atlas capture link`: the checkpoint linked to the
  Write session and was `sent`.
- Two bugs found and fixed live:
  - `file_changed` was missing because Claude's completed update carries
    only the status; sightings are now merged.
  - The MCP connection initialised before the token was bound, which logged
    an empty-id `session_start`; the grant is now read per request and
    empty ids are skipped.

Reference notes:

- evidence-acp.md §3.1 has the desktop send `clientCapabilities.session`.
  The acp-go-sdk schema (spec 0.13.5) has no such key, and the CLI sends it.
- evidence-acp.md §3.9 names `session/request_elicitation` and
  `session/complete_elicitation`; spec 0.13.5 names `elicitation/create` and
  `elicitation/complete`. Both are served.
- The schema describes prompt `usage` as "across all turns"; evidence §6 says
  claude-agent-acp's is per turn. The CLI follows the evidence: claude
  totals match per-turn sums.
- The quirk table (§2.4) is keyed `claude-code`, `codex` and `gemini`, but
  today's registry ids are `claude-acp`, `codex-acp` and `gemini`. Only the
  Gemini row can match. It is copied verbatim.

Not yet verified: the desktop checklist (`docs/acceptance/acp.md`). Not built
yet: the chat TUI (Phase 8). `atlas run` without `--prompt` says so.

## Phase 7 (finished) — every registry agent on this machine

The desktop checklist results have not arrived yet (the message carried a
`<paste>` placeholder), so nothing from it is fixed yet.

`ATLAS_ACP_TRACE=<file>` (new) records the **shape** of every message:
direction, method, id, error code, update kind, tool call id, status, and
the keys present. It never records a value. The evidence below comes from
it.

| agent | install | sign-in | file-editing prompt | notes |
|---|---|---|---|---|
| `claude-acp` 0.89.1 | npx (managed Node) | none needed: Claude Code's existing login | ✅ Write, Edit, Bash; `atlas_memory` used | `--resume` → `session/load` replayed history; the new turn was recorded as turn 2 of the same session |
| `codex-acp` 2.2.2 | npx | none needed: Codex's ChatGPT login was picked up | ✅ via shell (`cat > codex-notes.md`) | no permission prompts (Codex's own approval policy); model `gpt-6-luna`; usage per turn plus gauges |
| `gemini` 0.63.0 | npx | none needed: `GEMINI_API_KEY` in the env | ✅ write tool (after recovering), shell | two fixes below |
| `opencode` 1.18.21 | `--detected` (PATH, custom entry) | none needed | ✅ `write` tool → Write, file touch, patch | model `opencode/big-pickle`, usage plus gauges |

Every session, message, tool call and checkpoint reached `sent`.

Not run: the other 37 registry agents. None is on this machine (`atlas
agents detect` finds only opencode), and each needs an account of its
own. I did not create accounts or install them speculatively. The
terminal-auth path (AuthRequired → login command → authenticate "not
implemented" → retry) is covered only by the fixture agent, because no
installed agent asked to sign in.

Fixes from these runs (none per agent):

- **`fs/read_text_file` of a missing file now answers ACP's `-32002`
  "Resource not found"**, not `-32603`. Wire: gemini sent
  `fs/read_text_file` 4×, got `error:-32603` 4×, and its write tool failed.
  gemini-cli maps "Resource not found" / "ENOENT" / "No such file" in an
  error message to ENOENT. The evidence does not say what the desktop
  answers here; please check `handlers.rs:402-513` (evidence-acp.md:385).
- **Headless `--yes` picks `allow_once` before `allow_always`.** Gemini
  answered an `allow_always` selection with "privileged approval modes
  cannot be enabled in an untrusted folder". Allowing once never widens
  the agent's own approval mode. Without a terminal, `reject_once` is
  preferred too.
- Headless output no longer prints a `session/load` replay.
- `atlas agents install --detected` says where the copy is from.

Agent behaviour recorded, not changed:

- **codex-acp announces a shell call after running it.** Wire: `tool_call`
  `in_progress` at 23:56:56.934, `completed` at .937. The shell window's
  "before" snapshot already contains the new file, so after − before is
  empty: the call is captured, the file touch is not. The desktop takes
  its snapshot from the same notification, so it has the same gap.
- **gemini-cli's write tool still fails once for a new file** ("Error
  checking existing file: Resource not found"). Its
  `normalizeFileSystemError` only matches an `Error` instance, and its ACP
  client rejects with a plain object. It recovers (`touch`, then a
  successful write). Only per-agent code could avoid this.
- gemini sends no usage; `token_totals` stays `{}` apart from gauges.

## Phase 8 (first part) — the chat TUI

`atlas run <agent>` on a terminal opens the chat screen
(`internal/tui/chat`, Bubble Tea v2.1 with `charm.land/bubbles/v2` 2.2.1 for
the textarea and viewport, both pure Go). `--prompt` without `--no-tui`
pre-fills the input; `--no-tui` is the headless run. Sign-in runs before
the screen takes the terminal.

- **One theme file** (`internal/tui/theme.go`) holds every colour, as ANSI
  palette indices so the terminal's theme decides the shades. The old
  picker's colour literal moved there; nothing else names a colour.
- **Nothing branches on an agent id**: no file under `internal/tui` or
  `internal/run` names one. Every affordance is driven by capabilities and
  advertisements:
  - mode picker (ctrl+t) from `modes`, or a `category:"mode"` select;
  - model picker (ctrl+p) from a `category:"model"` select;
  - settings (ctrl+g) from `configOptions`, select and boolean;
  - slash picker from `available_commands_update`;
  - image attach (ctrl+o) only when `promptCapabilities.image`;
  - session list (ctrl+l) only when `sessionCapabilities.list`;
  - load or resume chosen by capability.
- Transcript:
  - streamed text and thinking (thinking folded to three lines, ctrl+e
    expands);
  - tool-call cards with a status glyph, kind, locations with line
    numbers, and diffs from `diff` content blocks (LCS, three lines of
    context, tested);
  - terminal output from client terminals and from
    `_meta.terminal_output`/`terminal_exit`;
  - a plan block updated in place; session titles as notices.
- Questions:
  - permission overlay with the agent's own options (name and kind);
  - elicitation forms built from `requestedSchema` in document order:
    string, number, integer, boolean, enum, required checks;
  - URL prompts (o opens a browser, enter accepts, ctrl+d declines);
  - `elicitation/complete` closes a URL prompt;
  - a question the agent withdraws (cancelled request) closes its overlay;
  - questions queue behind an open overlay.
- Turns: enter sends (alt+enter or ctrl+j for a newline). esc or ctrl+c
  cancels a running turn with the 5 s grace; ctrl+c on an empty input
  quits.
- Usage line: context used/size with a percentage, tokens per turn, cost,
  and "capture off" when the desktop holds the store.
- Error and disconnect: a disconnect shows the exit status and stderr
  tail. ctrl+r relaunches the agent and reopens the session (load or
  resume); ctrl+n starts a new session in the same process (`Agent.Rehost`).
- The bridge from the session to the program is one ordered, unbounded
  queue, so the ACP serial queue never waits on the screen. Session modes
  and config are now locked (the agent's queue writes, the screen reads).
- Tests: diff format; a fake driver covers streaming, cards, diff,
  terminal, plan, usage, the permission choice and withdrawal, an
  elicitation form with typed content, URL completion, slash completion,
  the mode and model pickers, the session list, resume, cancel, disconnect
  and restart, and attach gating. `-race` is clean.

Verified live: one full interactive `atlas run claude-acp` session through
a real pty (`expect`, 130×45), in `~/Codes/atlas-test-cloud`:

1. A prompt to write `tui-notes.md`. Claude's own permission options
   appeared ("Yes" / "Yes, allow all edits during this session" / "No").
   "allow once" was picked; the card went pending → completed with its
   diff; the file was written.
2. The usage line read `context 31.2k/1.0M (3%) · tokens in 4 · out 110 ·
   0.15 USD`, and the agent's title arrived as a notice.
3. The mode picker listed Claude's five modes (Manual, Accept edits, Plan,
   Auto, Bypass permissions), and the model picker its model list.
4. A follow-up question was answered in turn 2. ctrl+l listed five earlier
   sessions from `session/list`. ctrl+c quit.

Capture: one session with two completed turns, the Write call, file touch
`tui-notes.md`, usage plus gauges, all `sent`. Memory: `session_start`,
`file_changed`, `session_end`. The run file was removed on exit.

Not yet verified live: elicitation (no installed agent sent one), image
attach (claude-agent-acp advertises images, but no image was sent), and
restart after a real crash. A killed process (SIGKILL) leaves its turn
open, as on the desktop.

## Phase 9 — finish

- **Checkpoint rewrites** (`internal/capture/git/reconcile.go`), run by every
  link before the walk, deferred mid rebase, cherry-pick or merge:
  - reachability via `merge-base --is-ancestor`, then
    `for-each-ref --contains`, then `cat-file -e` (gone, or the probe failed);
  - an unreachable linked checkpoint with a patch-id is relinked to the one
    commit in `rev-list --all --max-count 200` with the same patch-id. When
    several match, the one on its recorded branch wins; otherwise it is
    orphaned (row kept). A relink onto an existing (session, commit) row
    is absorbed;
  - an orphan that becomes reachable again is recovered;
  - every change resyncs `sent → pending`;
  - `workspace_cursor.reconcile_note` gets `{"orphaned","relinked","failed","at"}`;
  - `atlas capture link` reports rewrites.

  Test: amend → relinked, reset away → orphaned plus the note, reset back
  → recovered.
- **`atlas status`**: account, org, Project and binding, gates, recorder,
  sessions (needing attention, turns left open), commits (cursor,
  recovered, orphaned checkpoints), sync (drain state; sent, pending and
  failed counts; a hint for retry and for 403), the memory record (entry
  count, chain check, "this machine only"), installed agents.
- **Memory browser**: `atlas memory browse` (Bubble Tea; kinds as tabs,
  `/` search, an entry with its evidence, `h` for revision history; read
  only) and `atlas memory list [--kind] [--search]` for scripts.
- **`atlas agents check [agent…|--all]`**: installs into the cache without
  touching the installed map, launches, runs `initialize`, tries
  `session/new` in an empty scratch dir, and reports the agent's info,
  protocol, capabilities, sign-in methods and the session outcome.
- **Sign-in for `agent`-kind methods**: the check showed cline,
  codebuddy-code, codex-acp and corust-agent offer only methods where the
  agent runs its own flow inside `authenticate`. Those are now
  authenticated directly (terminal methods still run their login command
  first). With several usable methods, the user picks on a terminal.
  Env-var methods are listed, never set.
- **Release**:
  - `.goreleaser.yaml`: linux/amd64, linux/arm64, darwin/arm64,
    `CGO_ENABLED=0`, `-X main.version={{ .Version }}`, tar.gz archives,
    sha256 `checksums.txt`, a draft GitHub release with `install.sh`
    attached;
  - `.github/workflows/release.yml`: on a `v*` tag, tests, then GoReleaser;
  - CI now also runs on pushes to `feat/**`;
  - `install.sh` (os/arch detection, latest tag or `ATLAS_VERSION`, sha256
    verification, `~/.local/bin`; refuses an Intel Mac, which has no v1
    build);
  - `make release-check` and `make snapshot`; `docs/RELEASE.md` with the
    procedure and four acceptance checklists (CLI smoke, cloud sync,
    desktop parity, agents).

  Verified locally: `goreleaser check` passes. The snapshot built all three
  archives and its `go mod tidy -diff` hook caught an untidy `go.sum`,
  since fixed. The linux/amd64 binary is static, `CGO_ENABLED=0`, and carries
  the version. `install.sh` against the snapshot installed it, refused a
  tampered checksum, and refused this Intel Mac with a clear message.
- **`atlas agents check --all`, every registry agent with a build for
  darwin-x86_64 (38 of 41)**: every one was installed for real (npx and
  binary archives) and launched.
  - **18 open a session at once**: amp-acp, claude-acp, codex-acp,
    cortex-code, crow-cli (34 s initialize and 28 s session/new when run
    alone; it timed out in the batch while competing for CPU), cursor,
    deepagents, devin, dirac, gemini, github-copilot-cli, glm-acp-agent,
    kilo, nova, opencode, sigit, stakpak, vtcode.
  - **15 answer `initialize` and need a sign-in** (AuthRequired on
    session/new): auggie, autohand, cline, codebuddy-code, corust-agent,
    dimcode, factory-droid, grok-build, junie, kimchi, minimax-code,
    mistral-vibe, poolside, qoder, qwen-code.
  - **5 fail on the agent's side**, none fixable without per-agent code:
    - agoragentic-acp answers `session/new` with `-32601 Method not found`
      (an MCP tool, not a coding agent);
    - antigravity-acp (darwin-x86_64 Mach-O) never answers `initialize`
      and writes nothing to stderr in 150 s;
    - goose answers `session/new` with a bare `-32603` (no details) until
      its provider is configured, instead of AuthRequired;
    - harn refuses `session/new` without a non-standard `environmentPolicy`
      parameter;
    - pi-acp needs the separate `pi` CLI installed ("executable not
      found").
  - **3 have no darwin-x86_64 build**: fast-agent, kimi, minion-code.
- Bugs found by the check and fixed:
  - **npm `bin` entries that are native executables** (factory-droid,
    grok-build) were run as `node <bin>` and died with a SyntaxError. A
    native binary (ELF/Mach-O) or a non-node `#!` script now runs
    directly; JavaScript still runs with the managed node. evidence-acp.md
    §1.4(c) says the command is always `node <executable>`, so the desktop
    likely has the same failure. Please check `node.rs:548-587` and
    `servers.rs:517-534`.
  - **The agent's stderr was lost on exit**: `exec.Cmd.Wait` closes the
    pipes it creates once the process exits. The CLI now uses its own
    `os.Pipe`s, so stderr and the final stdout are read to EOF. Test
    included.
  - Binary install directories were `0700` (the staging dir from
    `MkdirTemp`); now `0755`.
- **CI green on the remote**: commit `91fbc7c` pushed to
  `origin/feat/cli-foundation`; run 38131955335 passed lint and the tests on
  linux/amd64, linux/arm64 and darwin/arm64 (native runners). Annotation
  only: actions/checkout@v4, setup-go@v5, upload-artifact@v4 and
  golangci-lint-action@v8 target Node.js 20, which GitHub now forces onto
  Node 24. Bump them when newer majors exist.

Not done: no `v*` tag has been pushed, so the release workflow and the
published archives are untested on GitHub (verified locally with
`make snapshot`). The desktop checklist (`docs/acceptance/acp.md`) still
has no results.

## Release preparation (macOS and Linux)

- **ADR-0011**: v1 also ships for Intel Macs (darwin/amd64), the machine
  everything was verified on. GoReleaser builds four archives. CI tests
  darwin/amd64 natively on `macos-15-intel` alongside linux/amd64,
  linux/arm64 and darwin/arm64. `install.sh` installs on all four.
- CI and release actions moved to their Node 24 majors (checkout@v7,
  setup-go@v7, upload-artifact@v7, golangci-lint-action@v9,
  goreleaser-action@v7), which clears the Node 20 deprecation.
- **`install.sh` and the private repository**: anonymous release downloads
  fail while `tryatlas/atlas-cli` is private. The script now uses the
  GitHub CLI (`gh release view/download`, which handles auth) when it is
  signed in with access, and plain downloads otherwise. It never reads a
  token. The darwin/amd64 snapshot installed and ran natively here.
- **`atlas run --image <file>`** (repeatable, png/jpeg/gif/webp, ≤20 MiB,
  refused when the agent does not advertise images). The reader is shared
  with the chat screen. Verified live: `claude-acp` named the colour of an
  attached 64×64 PNG ("Red").
- **Restart after a crash**, verified live in the chat screen. Turn 1
  answered; `kill -9` of the agent's process group showed the disconnect
  with its stderr tail; ctrl+r relaunched it and reloaded the same session;
  turn 2 was recorded on that session; a clean exit followed. Also a
  deterministic test: the real `Chat` driver plus the fake agent, kill,
  `Restart`, `Prompt`, both turns on one session.
- **SIGTERM/SIGHUP** quit the chat screen cleanly. Verified live:
  `session_end` written, run file removed, no process left.
- **Stale run file**: a `cli-run.json` whose pid is dead (a SIGKILL or
  SIGQUIT) is ignored.
- **README**: the examples named `atlas run codex` and `atlas run
  claude-code`, which are not registry ids (`codex-acp`, `claude-acp`) and
  would fail. Fixed, with install and command sections added.

Still open, needing the user:

- the desktop checklist (`docs/acceptance/acp.md`);
- a license (there is no LICENSE file);
- whether the repository goes public (then `curl … | sh` works without
  `gh`);
- elicitation has not been seen from a real agent (none of the 38 sent
  one), so only the fixtures cover it.

## v0.1.0

- Commit `8505825` (Apache-2.0 LICENSE added). CI run 38133858890 is green
  on lint and on tests for linux/amd64, linux/arm64, darwin/arm64 and
  darwin/amd64, with no Node 20 warnings.
- PR #1 `feat/cli-foundation` → `main` is open for review.
- Tag `v0.1.0` on `8505825`. Release run 38134075354: tests, then GoReleaser.
  It produced a **draft** release with `atlas_0.1.0_{linux,darwin}_{amd64,arm64}.tar.gz`,
  `checksums.txt` and `install.sh`.
- Installed from the draft on this Intel Mac through `install.sh` (the `gh`
  path, private repository): `atlas 0.1.0`, a native x86_64 Mach-O.
  `atlas status` read the cloud Project; `atlas agents check claude-acp`
  answered with session ok.
- Published (no longer a draft), now the repository's latest release. Not done:
  merging PR #1, the
  desktop checklist.
