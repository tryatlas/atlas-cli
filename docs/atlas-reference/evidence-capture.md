# Atlas session capture ("Checkpoints") — on-disk format, pipeline and sync reference

Written for a Go CLI that must record agent sessions in Atlas's format and sync them with the Atlas backend, without reusing any Rust code. Every statement below is cited to the source as of commit `062699ab` on `feat/commit-tab-history` (2026-10-10). Paths are relative to `/Users/eemong/Codes/atlas`.

Scope: the `atlas-checkpoint` crate (`crates/atlas-checkpoint/`), its Tauri host (`src-tauri/src/commands/capture.rs`), the delta wire it consumes (`crates/atlas-agent-wire`, `crates/atlas-agent-delta`), the redactor (`crates/atlas-redact`), the profile crate (`crates/atlas-profile`), the importer, and every other writer of the project `.atlas/` folder.

---

## 0. One-paragraph summary

Capture is **opt-in per project**. When enabled, Atlas writes a SQLite database at `<project>/.atlas/sessions.db` (schema version 10) plus a content-addressed blob sidecar `<project>/.atlas/blobs/`. One **Session** row per agent conversation; one **Message** row per finalized assistant/thinking message and per user prompt; one **tool_call** row per tool invocation; **file_touch** and **agent_edit** rows per file the agent wrote; a **turn** row per prompt; a **Checkpoint** row per `(Session, git commit)` pair the link rule associates. **All agent content is redacted before it is written** (`crates/atlas-checkpoint/src/capture.rs:1-18`). Commits are linked by observing git refs move — never by hooks — through an asymmetric path/content rule, and links survive rebases via `git patch-id --stable`. Cloud sync is a per-row `sync_state` outbox drained to `https://ingest.tryatlas.cc` with a bearer JWT (`POST /ingest`, `PUT /blobs/{sha256}`); **Local mode is the same database with draining switched off** (`crates/atlas-checkpoint/src/lib.rs:19-25`).

---

## 1. Vocabulary (exact meanings)

| Term | Meaning | Source |
|---|---|---|
| **Project** | A folder opened in Atlas. The unit capture, the git watcher, the file index and the knowledge base are scoped to. *Avoid: workspace* — but the storage keys still say `workspace_id` (the `agent_session.workspace_id`, `binding.workspace_id`, `workspace_cursor.workspace_id` columns) and renaming them is a data migration. | `CONTEXT.md:28` |
| **Session** | One agent conversation as capture records it: one `agent_session` row, identified by `(workspace_id, source, native_session_id)`. "Recorded session" is the Organisation-facing name for the same thing. *The ACP sense*: the agent-side conversation identified by an ACP session id — that id is what capture stores as `native_session_id`. | `crates/atlas-checkpoint/src/lib.rs:14-15`, `src/model.rs:353-395`, `CONTEXT.md:9,123` |
| **Checkpoint** | "The slice of one Session whose work landed in one git commit — identified by the `(Session, commit)` pair, and carrying no transcript of its own." One `checkpoint` row per `(session_id, commit_sha)`. Two Sessions contributing to one commit produce two Checkpoints; one Session spanning five commits produces five. | `src/lib.rs:15-17`, `src/schema.rs:476-486` |
| **Turn** | One prompt and the answer to it. One `turn` row per `(session_id, turn_seq)`, states `open` → `completed` / `aborted` / `rewound`. `turn_seq` is minted by the host at send time, 1-based per Session, seeded from the store's `MAX(turn_seq)` after a restart. | `CONTEXT.md:20`, `src/model.rs:189-236`, `src-tauri/src/commands/capture.rs:114-119,609-626` |
| **Prompt** | The user's message, **as typed**, before Atlas prepends any injected context. It is *not* on the delta stream; it enters capture via an explicit `note_prompt` call on the send path. Stored as a `role='user', mode='text'` Message. | `crates/atlas-checkpoint/src/capture.rs:110-121`, `src-tauri/src/commands/capture.rs:18-25` |
| **Message** | One user or assistant row within a turn: one `agent_message` row with `role ∈ {user, assistant, system}` and `mode ∈ {text, tool, thinking}`. The **only** place transcript text lives in the store. | `src/schema.rs:298-321`, `src/model.rs:397-418` |
| **Transcript** | Three distinct things. (a) **An agent's own transcript** on disk — Claude Code's `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`; the checkpoint **importer** reads these as `source = external_jsonl`. (b) **Atlas's own transcript** — `<app-config-dir>/agent-transcripts/<cwd-hash>/<session-id>.json`, text-only, written for every agent by `TranscriptMiddleware`; used for sidebar preview/replay, never for capture. (c) The `atlas-agent-transcript` crate — only helpers (`encode_cwd`, directory discovery, envelope stripping). **A capture is none of these**: it is the redacted `sessions.db` record with tool calls, file touches, edits, usage and Checkpoints. | `crates/atlas-checkpoint/src/import.rs:1-14`, `src-tauri/src/commands/agent_transcript.rs:1-24,268-275`, `crates/atlas-agent-transcript/src/lib.rs:1-29` |
| **Thread** | One conversation as the sidebar tracks it, keyed by an **Atlas-minted `ThreadId`** (UUID v4, hyphenated), stored in `<app-config-dir>/threads.db` (`atlas-thread-metadata`). Metadata only; references at most one ACP session id. **Capture never references a ThreadId** — it keys on the agent's own session id. | `CONTEXT.md:7-10`, `crates/atlas-thread-metadata/src/model.rs:26-52,82-113`, `docs/adr/0001-app-owned-thread-metadata-store.md:21-23` |
| **Source** | Where a Session came from: `acp` (ACP-hosted agent inside Atlas), `native` (Atlas Agent), `external_jsonl` (imported from the agent's own transcript). Only `acp`/`native` are "live" and eligible for Checkpoints. | `src/model.rs:10-52` |
| **Binding** | How a Project is bound: `mode ∈ {local, cloud}`, `enabled`, org/slug/remote id, git fingerprint. One singleton row (`binding.id = 1`). | `src/model.rs:521-572`, `src/schema.rs:546-577` |
| **Organisation / Workspace (cloud)** | The tenant and the cloud aggregate a Project is bound to. The server-assigned Project id is `binding.remote_workspace_id`; it is the wire identity on every synced artifact, with `binding.slug` as the fallback when it is unset (`remote_workspace_id.or(slug)`, `src-tauri/src/commands/capture.rs:3376-3381`). | `CONTEXT.md:122-127`, `src/model.rs:549-552` |
| **Timeline / Artifacts** | The read model over the store (`SessionSummary`, `SessionDetail`, `TimelineEntry{kind ∈ prompt, response, thinking, tool_call, checkpoint}`). | `crates/atlas-checkpoint/src/timeline.rs:1-27,114-126` |
| **Capture health** | One state per Project (`off`, `ok`, `degraded`, `stopped`) computed from the store plus host signals. | `crates/atlas-checkpoint/src/health.rs:32-47` |

---

## 2. Identity model

### 2.1 Project id (`workspace_id`) — the canonical absolute path

- `project_id_for(root) = dunce::canonicalize(root).unwrap_or(root).to_string_lossy()` — symlinks resolved, no trailing slash, `/private/var/...` not `/var/...` on macOS. Used for **live Session rows** (`SessionKey.workspace_id`) and the commit walk's `workspace_id`. `src-tauri/src/commands/capture.rs:74-93,704,3123`.
- `binding.workspace_id` and `binding.root` are written from the **`project_path` string the frontend passes** to `capture_enable` (`src-tauri/src/commands/capture.rs:1236`; `crates/atlas-checkpoint/src/binding.rs:80-91`). The frontend canonicalises the cwd first (`canonical_project_dir`, `src-tauri/src/commands/agents.rs:1664`; ADR-0001 "One spelling of the project", `docs/adr/0001-...md:41`).
- The **importer** keys imported Sessions under `root.to_string_lossy()` (the path as given), `src-tauri/src/commands/capture.rs:3295`. Memory's reader therefore tries both spellings (`src-tauri/src/commands/memory_capture.rs:91-100`).
- The **drain** selects pending rows by `root.to_string_lossy()` (`capture.rs:3363,3388`). So for rows to drain, they must be keyed by the same string Atlas's frontend passes — i.e. the canonical path.
- On folder rename, `open_in` compares `binding.workspace_id` with the current path and re-keys `agent_session`, `workspace_cursor` and `binding` in one transaction (`capture.rs:2900-2918`; `store.rs:2015-2048`).

**Rule for the CLI:** always use one spelling — the canonicalised absolute project root with no trailing slash — for `binding.workspace_id`, `binding.root`, `agent_session.workspace_id` and `workspace_cursor.workspace_id`.

### 2.2 Session identity

- `agent_session.id` = `"as-" + uuid_v4_simple` (32 lowercase hex, no hyphens), minted by the store (`store.rs:238`).
- `agent_session.native_session_id` = the agent's own id: the **ACP session id** for ACP agents (also the Claude JSONL file stem), the engine's session id for the native agent. Capture is told it at `note_prompt(session_id, cwd, plugin_id, model, prompt)` (`capture.rs:595-602`; caller at `src-tauri/src/commands/agents.rs:1764-1770`).
- `UNIQUE (workspace_id, source, native_session_id)` (`schema.rs:295`). Dedupes re-imports within a source only; `('acp', id)` and `('external_jsonl', id)` may both exist and the importer skips the cross-source duplicate explicitly (`import.rs:26-31,357-369`).
- `agent_session.agent` = the plugin id string: `"claude-code"`, `"codex"`, or the native agent's literal id `"atlas-agent"` (`ATLAS_AGENT_ID`; `CONTEXT.md:29`; `capture.rs:636,2834-2840`). `source = native` iff `plugin_id == ATLAS_AGENT_ID`.
- Shared memory joins a recorded session by this same `native_session_id` (`src-tauri/src/commands/memory_capture.rs:1-9,62-88`).

### 2.3 Row id prefixes (the drain depends on them)

`as-` agent_session, `am-` agent_message, `tc-` tool_call, `cp-` checkpoint, `ft-` file_touch, `ae-` agent_edit; all `<prefix><uuid v4 simple>` (`store.rs:238,764,1326,1366,1395,2165,2836-2843`).

### 2.4 Thread id (ADR-0001)

`ThreadId = uuid::Uuid::new_v4()`, serialised hyphenated (`crates/atlas-thread-metadata/src/model.rs:32-52`). Lives in `<app-config-dir>/threads.db` (`crates/atlas-thread-metadata/src/lib.rs:71-72`). Row: `thread_id, session_id (nullable ACP id), agent_id, title, title_override, updated_at, created_at, interacted_at, worktree_paths, remote_connection, branch, archived` (`model.rs:84-113`). Not consulted by capture.

### 2.5 Other per-project ids (not capture's)

- `.atlas/project.json` → `{"projectId": "<first 12 hex of sha256(cwd with trailing '/' trimmed)>"}` written by **shared memory** when it first appends an event (`src-tauri/src/commands/shared_memory.rs:557-581`). Not read by capture.
- Claude's transcript folder slug: `encode_cwd` — every UTF-16 unit outside `[A-Za-z0-9]` → `-`; over 200 units → cut + `-` + base36(|java hash|) (`crates/atlas-agent-transcript/src/lib.rs:51-97`).
- Atlas's own transcripts live under `<app-config-dir>/agent-transcripts/<cwd_hash(cwd)>/` (`src-tauri/src/commands/agent_transcript.rs:268-275`).

### 2.6 Org binding

`binding.mode='cloud'`, `org_id`, `slug`, `remote_workspace_id` (server-assigned Project id), `drain_state ∈ {ok, not_authorized}`, `import_approved` (`schema.rs:557-577,595-610`; `store.rs:1608-1624`). Set only by `capture_register_cloud` / `capture_connect` / `capture_promote` after the server call succeeds (`capture.rs:2247-2322,2408-2495,2701-2762`).

### 2.7 Profile: `.atlas` vs `.atlas-dev`

Every path goes through `atlas_profile::dir_in(root)` = `<root>/.atlas` (release, identifier `dev.atlas.ide`) or `<root>/.atlas-dev` (dev build, identifier `dev.atlas.ide.dev`); `~/.config/atlas` vs `~/.config/atlas-dev` (`crates/atlas-profile/src/lib.rs:11-15,103-122,169-171`). A CLI targeting the shipped app writes `.atlas/`.

---

## 3. On-disk layout written by capture

```
<project-root>/.atlas/
├── .gitignore            "*" — makes the dir ignore itself              store.rs:2861-2872
├── sessions.db           SQLite, WAL mode, user_version = 10            store.rs:98-143
├── sessions.db-wal       SQLite WAL sidecar (present while open)
├── sessions.db-shm       SQLite shared-memory sidecar
├── sessions.lock         writer lock: a tiny SQLite db held in BEGIN EXCLUSIVE   lock.rs:27-61, store.rs:89
├── blobs/<hh>/<sha256>   spilled payloads > 64 KiB, content-addressed    blobs.rs:27-104
├── import-resume.json    importer resume cache (map path → ResumeState)  import.rs:299-333
└── .write-probe          transient health probe, created then deleted    store.rs:2566-2572
```

### 3.1 `sessions.db` — connection settings

`SQLITE_OPEN_READ_WRITE | SQLITE_OPEN_URI | SQLITE_OPEN_NO_MUTEX` (+`CREATE` for the writer); `PRAGMA journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`, `busy_timeout=5000ms` (`store.rs:117-143`). Schema version in `PRAGMA user_version`; a store with `user_version > 10` is refused (`SchemaTooNew`) (`schema.rs:26-40`). Migrations run inside `BEGIN IMMEDIATE` (`schema.rs:51-112`). The dev-only "withdrawn 9" quirk is at `schema.rs:18-25,85-95`.

### 3.2 Writer lock (`sessions.lock`)

`Connection::open(".atlas/sessions.lock")`, `busy_timeout(0)`, `BEGIN EXCLUSIVE`; held for the life of the store; `SQLITE_BUSY`/`SQLITE_LOCKED` → `Error::AlreadyLocked` and the process attaches **read-only** (`lock.rs:38-61`; `store.rs:88-96,173-175`). Every write method calls `require_writer()` (`store.rs:2617-2623`); the exceptions are `flag_needs_attention` (`store.rs:648-659`) and `add_redaction_counts` (`store.rs:662-692`). **A CLI holding this lock makes Atlas defer capture and report "Another Atlas window is recording this Project"** (`health.rs:175-182`; `capture.rs:2889-2898`). Conversely the CLI must not write while Atlas holds it.

### 3.3 Full schema (verbatim, `crates/atlas-checkpoint/src/schema.rs`)

V1 (`schema.rs:268-368`):

```sql
CREATE TABLE IF NOT EXISTS agent_session (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL,
    source            TEXT NOT NULL,
    native_session_id TEXT NOT NULL,
    title             TEXT,
    agent             TEXT,
    model             TEXT,
    cwd               TEXT,
    token_totals      TEXT NOT NULL DEFAULT '{}',
    summary           TEXT,
    started_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    needs_attention   INTEGER NOT NULL DEFAULT 0,
    attention_reason  TEXT,
    redaction_counts  TEXT NOT NULL DEFAULT '{}',
    sync_state        TEXT NOT NULL DEFAULT 'local',
    sync_attempts     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (workspace_id, source, native_session_id)
);
CREATE TABLE IF NOT EXISTS agent_message (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    seq               INTEGER NOT NULL,
    turn_seq          INTEGER NOT NULL,
    native_message_id TEXT,
    role              TEXT NOT NULL,
    mode              TEXT NOT NULL,
    preview           TEXT NOT NULL,
    body              TEXT,
    body_ref          TEXT,
    body_bytes        INTEGER NOT NULL DEFAULT 0,
    content_hash      TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    sync_state        TEXT NOT NULL DEFAULT 'local',
    sync_attempts     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, seq)
);
CREATE TABLE IF NOT EXISTS turn (
    session_id TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq   INTEGER NOT NULL,
    state      TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    PRIMARY KEY (session_id, turn_seq)
);
CREATE TABLE IF NOT EXISTS counter (
    name  TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
INSERT OR IGNORE INTO counter (name, value) VALUES ('seq', 0);
CREATE INDEX IF NOT EXISTS idx_session_outbox      ON agent_session (workspace_id, sync_state);
CREATE INDEX IF NOT EXISTS idx_message_outbox      ON agent_message (sync_state, seq);
CREATE INDEX IF NOT EXISTS idx_message_session_seq ON agent_message (session_id, seq);
CREATE INDEX IF NOT EXISTS idx_message_facets      ON agent_message (session_id, role, mode);
CREATE UNIQUE INDEX IF NOT EXISTS idx_message_native_id
    ON agent_message (session_id, native_message_id) WHERE native_message_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_session_started     ON agent_session (workspace_id, started_at);
```

V2 (`schema.rs:370-473`):

```sql
CREATE TABLE IF NOT EXISTS tool_call (
    id             TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    turn_seq       INTEGER NOT NULL,
    native_call_id TEXT,
    tool_name      TEXT NOT NULL,
    title          TEXT,
    kind           TEXT,
    status         TEXT NOT NULL,
    locations      TEXT NOT NULL DEFAULT '[]',
    arguments      TEXT,
    arguments_ref  TEXT,
    result         TEXT,
    result_ref     TEXT,
    result_binary  INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    sync_state     TEXT NOT NULL DEFAULT 'local',
    sync_attempts  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, seq)
);
CREATE TABLE IF NOT EXISTS file_touch (
    id             TEXT PRIMARY KEY,
    tool_call_id   TEXT NOT NULL REFERENCES tool_call(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq       INTEGER NOT NULL,
    seq            INTEGER NOT NULL,
    path           TEXT NOT NULL,
    sha256_after   TEXT,
    existed_before INTEGER NOT NULL,
    deleted        INTEGER NOT NULL DEFAULT 0,
    out_of_repo    INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_edit (
    id           TEXT PRIMARY KEY,
    tool_call_id TEXT NOT NULL REFERENCES tool_call(id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq     INTEGER NOT NULL,
    path         TEXT NOT NULL,
    patch        TEXT,
    patch_ref    TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tool_call_facets      ON tool_call (session_id, tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_call_session_seq ON tool_call (session_id, seq);
CREATE INDEX IF NOT EXISTS idx_tool_call_outbox      ON tool_call (sync_state, seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_call_native_id
    ON tool_call (session_id, native_call_id) WHERE native_call_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_file_touch_path    ON file_touch (session_id, path);
CREATE INDEX IF NOT EXISTS idx_file_touch_by_path ON file_touch (path);
CREATE INDEX IF NOT EXISTS idx_agent_edit_session ON agent_edit (session_id, turn_seq);
```

V3 (`schema.rs:475-544`):

```sql
CREATE TABLE IF NOT EXISTS checkpoint (
    id              TEXT PRIMARY KEY,
    session_id      TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    commit_sha      TEXT NOT NULL,
    patch_id        TEXT,
    link_state      TEXT NOT NULL DEFAULT 'linked',
    branch          TEXT,
    git_author_name  TEXT,
    git_author_email TEXT,
    files_touched   TEXT NOT NULL DEFAULT '[]',
    insertions      INTEGER NOT NULL DEFAULT 0,
    deletions       INTEGER NOT NULL DEFAULT 0,
    attribution     TEXT,
    created_at      TEXT NOT NULL,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    sync_attempts   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, commit_sha)
);
CREATE TABLE IF NOT EXISTS workspace_cursor (
    workspace_id     TEXT PRIMARY KEY,
    last_seen_commit TEXT,
    recovered        INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_checkpoint_commit  ON checkpoint (commit_sha);
CREATE INDEX IF NOT EXISTS idx_checkpoint_session ON checkpoint (session_id);
CREATE INDEX IF NOT EXISTS idx_checkpoint_patch   ON checkpoint (patch_id);
CREATE INDEX IF NOT EXISTS idx_checkpoint_outbox  ON checkpoint (sync_state);
```

V4 (`schema.rs:546-577`):

```sql
CREATE TABLE IF NOT EXISTS binding (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    workspace_id    TEXT NOT NULL,
    root            TEXT NOT NULL,
    mode            TEXT NOT NULL DEFAULT 'local',
    slug            TEXT,
    org_id          TEXT,
    root_commit_sha TEXT,
    fingerprint_is_shallow INTEGER NOT NULL DEFAULT 0,
    git_url         TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
```

V5 (`schema.rs:579-593`):

```sql
CREATE TABLE IF NOT EXISTS import_progress (
    path          TEXT PRIMARY KEY,
    imported_size INTEGER NOT NULL,
    updated_at    TEXT NOT NULL
);
```

V6 (`schema.rs:595-626`), applied statement-by-statement, tolerant of "duplicate column name" (V7, V8 and V9 use the same `apply_tolerant` path, and V9 is re-run on a store already stamped 9: `schema.rs:75-95,134-148`):

```sql
ALTER TABLE binding ADD COLUMN import_approved INTEGER NOT NULL DEFAULT 0;
ALTER TABLE binding ADD COLUMN drain_state TEXT NOT NULL DEFAULT 'ok';
ALTER TABLE binding ADD COLUMN remote_workspace_id TEXT;
ALTER TABLE file_touch ADD COLUMN consumed_by_commit TEXT;
CREATE INDEX IF NOT EXISTS idx_file_touch_unconsumed
    ON file_touch (session_id, path) WHERE consumed_by_commit IS NULL;
ALTER TABLE workspace_cursor ADD COLUMN reconcile_note TEXT;
```

V7 (`schema.rs:150-159`): `ALTER TABLE agent_session ADD COLUMN branch TEXT;`

V8 (`schema.rs:161-201`):

```sql
ALTER TABLE agent_session ADD COLUMN last_activity_at TEXT;
UPDATE agent_session SET last_activity_at = (
    SELECT MAX(stamp) FROM (
        SELECT MAX(created_at) AS stamp FROM agent_message WHERE session_id = agent_session.id
        UNION ALL
        SELECT MAX(ended_at) AS stamp FROM turn WHERE session_id = agent_session.id
    )
);
UPDATE agent_session SET last_activity_at = updated_at WHERE last_activity_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_session_activity ON agent_session (workspace_id, last_activity_at);
CREATE INDEX IF NOT EXISTS idx_message_activity ON agent_message (session_id, created_at);
DELETE FROM import_progress;
```

V9 (`schema.rs:203-220`): `ALTER TABLE file_touch ADD COLUMN sketch_after TEXT;`

V10 (`schema.rs:222-266`):

```sql
CREATE TABLE IF NOT EXISTS usage_delta (
    session_id            TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq              INTEGER NOT NULL,
    model                 TEXT,
    recorded_at           TEXT NOT NULL,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, turn_seq)
);
CREATE TABLE IF NOT EXISTS usage_cursor (
    session_id            TEXT PRIMARY KEY REFERENCES agent_session(id) ON DELETE CASCADE,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_delta_recorded ON usage_delta (recorded_at);
```

Required indexes a reader may assert on: `REQUIRED_INDEXES` (`schema.rs:631-653`).

### 3.4 Column value formats and enums

| Column | Format | Source |
|---|---|---|
| all `*_at`, `recorded_at` | RFC 3339 string from chrono `DateTime<Utc>::to_rfc3339()` — `YYYY-MM-DDTHH:MM:SS[.fffffffff]+00:00` (sub-seconds printed only when non-zero, in 3/6/9-digit groups). Parsed back with `parse_from_rfc3339`; unparseable → epoch. **The store compares these lexically in SQL** (`last_activity_at < ?3`, `created_at <= ?4`, `MAX(created_at)`), so the CLI must emit UTC with the `+00:00` suffix, never `Z`, to sort consistently. | `store.rs:233,253,789-806,2538,2989-2993` |
| `agent_session.source` | `acp` \| `native` \| `external_jsonl` \| `proxy` (added 2026-10-10 for the Atlas CLI's proxy-captured sessions; `is_live` includes it) | `model.rs:26-33` |
| `agent_message.role` | `user` \| `assistant` \| `system` | `model.rs:62-69` |
| `agent_message.mode` | `text` \| `tool` \| `thinking` | `model.rs:95-102` |
| `*.sync_state` | `local` \| `pending` \| `sent` \| `failed` | `model.rs:133-141` |
| `binding.mode` | `local` \| `cloud` | `model.rs:164-170` |
| `binding.drain_state` | `ok` \| `not_authorized` | `model.rs:584-591` |
| `turn.state` | `open` \| `completed` \| `aborted` \| `rewound` | `model.rs:207-216` |
| `tool_call.status` | `pending` \| `running` \| `completed` \| `failed` | `model.rs:442-450` |
| `tool_call.tool_name` | canonical, **capitalised**: `Read` `Edit` `Write` `Bash` `Search` `Fetch` `Delete` `Move` `Think` `Task` `Other` | `tools.rs:45-60` |
| `checkpoint.link_state` | `linked` \| `orphaned` | `model.rs:635-642` |
| `agent_session.token_totals` | JSON `{"input_tokens":u64,"output_tokens":u64,"cache_creation_tokens":u64,"cache_read_tokens":u64,"reasoning_tokens":u64,"context_used"?:u64,"context_size"?:u64}` (all `#[serde(default)]`; gauge fields omitted when `None`) | `model.rs:256-279` |
| `agent_session.redaction_counts` | JSON object `{ "<category>": count }` with categories `entropy`, `vendor_rule`, `provider_token`, `credentialed_uri`, `connection_string`, `credential_value` | `store.rs:661-692`; `crates/atlas-redact/src/lib.rs:80-90` |
| `tool_call.locations` | JSON array as the wire gave it (`[{"path": "...", ...}]`); `'[]'` means "no news" on update | `store.rs:1275,1299` |
| `checkpoint.files_touched` | JSON array of commit-side project-relative paths, sorted, deduped | `store.rs:2180`; `checkpoint.rs:417-418` |
| `checkpoint.attribution` | always NULL (reserved) | `model.rs:667-671` |
| `agent_session.summary` | always NULL (reserved) | `model.rs:377-378` |
| `workspace_cursor.reconcile_note` | JSON `{"orphaned":n,"relinked":n,"failed":n,"at":"<rfc3339>"}` | `checkpoint.rs:708-732` |
| `agent_message.content_hash`, `blob keys`, `file_touch.sha256_after` | lowercase hex SHA-256 of the (redacted, for bodies) bytes | `blobs.rs:111-116`; `store.rs:744`; `capture.rs(crate):78-80` |
| `file_touch.sketch_after` | comma-joined lowercase-hex FNV-1a 32-bit hashes of distinct trimmed non-blank lines, sorted ascending, deduped, truncated to 512 entries; NULL for empty/blank/binary content | `sketch.rs:38-98` |
| `file_touch.path`, `agent_edit.path` | project-relative, `/`-separated, `.`/`..` collapsed lexically (no symlink resolution), composed via a fixed 26-pair table over 7 Latin-1 combining marks (`tools.rs:303-361`; not full NFC — `ì ù ò ý ÿ À È Ì` are not composed); absolute path kept verbatim when `out_of_repo=1` | `tools.rs:225-361` |
| `agent_message.native_message_id` | the wire message id for assistant/thinking rows; `prompt-{turn_seq}-{sha256hex(prompt)}` for live prompts; the transcript line `uuid` or `line-{index}-{sha256hex(line)}` for imports | `capture.rs(crate):150-154`; `import.rs:571-574,996-998` |
| `tool_call.native_call_id` | the wire tool-call id (ACP `toolCallId`, or Claude `tool_use.id`) | `capture.rs(host):4041`; `import.rs:615,648` |
| `agent_message.seq`, `tool_call.seq`, `file_touch.seq` | one store-wide monotonic counter (`counter.value`), incremented inside the same transaction as the row | `store.rs:2914-2929` |

### 3.5 Size limits and spill

- `SPILL_THRESHOLD_BYTES = 64 * 1024`: a body/arguments/result/patch **strictly larger** than 64 KiB goes to `blobs/` and the row keeps `*_ref = <sha256>` with `body = NULL` (`blobs.rs:19-22,118-121`; `store.rs:737-745,1268-1273,1509-1518`).
- `PREVIEW_BYTES = 2 * 1024`: `agent_message.preview` is the first ≤2 KiB cut on a char boundary, **always** stored (`blobs.rs:24-25,125-134`).
- `body_bytes` = length of the redacted body in bytes (`store.rs:737`).
- Blob file: `blobs/<key[0..2]>/<key>`; written to `<key>.tmp`, `fsync`, rename, best-effort dir fsync; `put` is a no-op if the file exists; `get` deletes and errors on a hash mismatch (`blobs.rs:37-104`).
- Title: ≤120 chars, ellipsis `…` reserved in the budget (`title.rs:14-16,288-306`).
- Binary (non-UTF-8) tool results: stored verbatim as a blob with `result_binary=1`, **never** redacted (`capture.rs(crate):294-307`; `store.rs:1269-1274`).
- No rotation and no blob GC exist. `import-resume.json` entries over 1,000,000 bytes are dropped (`import.rs:294-297`).
- Timeline inlining limit (read side) is also 64 KiB (`timeline.rs:36-40`).

### 3.6 `import-resume.json`

Map `transcript absolute path → ResumeState` where `ResumeState = {offset: u64, line_count: usize, turn_seq: i64, earliest: rfc3339|null, model_seen: string|null, usage_by_request: {requestId: TokenTotals}, call_meta: {tool_use_id: [ToolName, turn_seq]}}` (`import.rs:283-333`). Pure cache; a CLI may ignore or delete it.

---

## 4. The capture pipeline

### 4.1 Where capture sits

Every delta travels the ordered `OutboundPipeline`: `BroadcastMiddleware` → `AnalyticsMiddleware` → `KeepAwakeMiddleware` → **`CaptureMiddleware`** → `TranscriptMiddleware` → `MemoryIngestMiddleware` (`src-tauri/src/commands/agents.rs:79-100`). Capture is a synchronous pipeline stage (cannot lag/drop), which only **enqueues** `Job`s to one owned worker thread `atlas-capture`; all SQLite writes happen there, in order (`capture.rs(host):10-31,122-189,454-474,2934-2999`). A second thread `atlas-capture-sync` does cloud drains and transcript imports on a sibling connection (`capture.rs(host):387-414,486-499,3457-3553`).

### 4.2 Gates checked per job, in order (`capture.rs(host):3002-3118`)

1. `enabled_on_disk(root)` = `.atlas/sessions.db` exists; otherwise the job is dropped **without creating the store** (`:2842-2851,3022-3030`).
2. Open the process-wide writer (`open_in`); if the writer lock is held by another process, drop (`:3032-3037`).
3. `binding` row must exist (`:3041-3043`).
4. `WalkCommits` jobs always reconcile; they walk only if `binding.enabled`; every other job requires `binding.enabled` (`:3044-3114`).
5. A `Turn`/`ToolCall`/`FinishTurn`/`RewindTurns`/`Usage` job for a session whose `Prompt` has not been processed in this process is silently ignored (`session_ids` map) (`:3160-3177,3191-3235`).

### 4.3 Order of events for one turn (what happens, and the exact rows)

**Step 0 — prompt, on the send path, before any memory injection** (`agents.rs:1751-1770` → `capture.rs(host):595-661`):

- Read `HeadMark` (= `git rev-parse HEAD`, or `Unborn`) and remember it per session as the "turn anchor" (`:616-620,246-268`).
- Seed `turn_seq` from `SELECT COALESCE(MAX(turn_seq),0) FROM turn WHERE session_id=?` on first sighting; then `turn_seq += 1` (`:621-644`; `store.rs:1005-1011`).
- Resolve `branch = git symbolic-ref --quiet --short HEAD` once per session (re-asked while unknown) (`:640,651-653`).
- Worker `Job::Prompt` → `Capture::record_prompt` (`capture.rs(crate):122-168`):
  1. `upsert_session(workspace_id, source, native_session_id, agent, model, branch=None, cwd, mode)` — insert `agent_session` with `started_at = updated_at = now`, `sync_state = mode.initial_sync_state()` (`local` → `local`, `cloud` → `pending`); on an existing row `agent = COALESCE(new, old)`, `model = COALESCE(new, old)` (**new wins**), `branch = COALESCE(old, new)` (**first wins**), `cwd = COALESCE(new, old)`, `updated_at = now` (`store.rs:203-258`).
  2. `begin_turn` → `INSERT INTO turn (…, 'open', now) ON CONFLICT DO NOTHING` (`store.rs:697-706`).
  3. Derive the title from the **redacted** prompt and `set_title_if_absent` (first prompt wins; `sent` → `pending` resync) (`store.rs:322-332`; see §4.7).
  4. `record_content` with `native_message_id = "prompt-{turn_seq}-{sha256(prompt)}"`, `role=user`, `mode=text`, `created_at = now`: strip host machinery (§4.8), redact (§5), spill if needed, `INSERT INTO agent_message`, bump `agent_session.updated_at = now` and `last_activity_at = max(last_activity_at, created_at)`; one transaction (`store.rs:729-811`).
  5. `note_branch` → `UPDATE agent_session SET branch=? WHERE id=? AND branch IS NULL` (`store.rs:2259-2273`).

**Step 1 — `status: running`**: ignored by capture (turn start is the prompt) (`capture.rs(host):4108`).

**Step 2 — `message_appended`** (assistant/system only; user messages never arrive on the wire and are dropped if they did): start an in-memory `PendingMessage{id, role, mode, body=first chunk, started_at=now}` under the session's `PendingTurn`, which snapshots the `SessionBinding` (turn_seq) as of the turn's first message (`:3898-3926,716-748`). `mode: thinking` takes `message.thinking`; `text`/`tool` take `message.content`.

**Step 3 — `text_chunk` / `thinking_chunk`**: appended to the pending message with that `message_id` (`:3928-3931,755-762`). Nothing is written yet.

**Step 4 — `tool_call_upserted`** (every sighting, pending → running → completed/failed) (`:3933-4053`):

- `status` mapped 1:1; `terminal = completed | failed`.
- `tool_name = canonical_name(wire tool_name, title, kind, arguments)` (§4.9).
- Write sampling (§4.10) if `tool_name.writes_files()` or the call carries a `diff` content block; shell window (§4.11) if `tool_name == Bash`; else nothing.
- On the **first** terminal sighting with `status == completed`, each sampled path is read **on the emit thread now** (`std::fs::read(project_root/path)`): `sha256_after = sha256(bytes)`, `sketch_after = sketch(bytes)`, `deleted = read failed` (`:3987-4028`). A **failed** call records no writes (`:3994-4002`).
- `patch = edit_patch(arguments, content_blocks, first write path, root)` (§4.12).
- `Job::ToolCall{native_call_id = tool_call.id, tool_name, title, kind, status, locations = JSON array, arguments = serialize_arguments (None for `{}`/null), result = tool_call.result, writes, settled_commits, patch}` (`:4039-4052,3793-3799`).
- Worker (`:3741-3789`): `Capture::record_tool_call` → redact `arguments` and `result` (JSON-aware), then `upsert_tool_call`: if `(session_id, native_call_id)` exists → `UPDATE … SET tool_name=?, title=COALESCE(new,old), kind=COALESCE(new,old), status=?, locations = CASE WHEN new='[]' THEN old ELSE new END, arguments=COALESCE(new,old), arguments_ref=COALESCE(new,old), result=COALESCE(new,old), result_ref=COALESCE(new,old), result_binary=CASE WHEN new=1 THEN 1 ELSE old END`; else `INSERT` with `created_at = now`, `seq = next_seq` (`store.rs:1262-1355`). Then one `file_touch` row per write (`store.rs:1362-1389`), and at most one `agent_edit` row per call, stored against the **first** write's path (`:3777-3787`).
- If `settled_commits` is non-empty, `link_commits` evaluates exactly those commits after the touches land (`:3244-3258`; §6.6).

**Step 5 — `usage_updated`** → `Job::Usage{TokenTotals{input, output, cache_creation, cache_read, reasoning}}` (native agent's real split) (`:4055-4068`). **`context_usage`** → `Job::Usage{context_used, context_size}` only (`:4070-4083`). Worker → `record_usage_delta(session_id, binding.turn_seq, binding.model, totals)`: one transaction that (a) computes per-field deltas against `usage_cursor` (reported `0` = not reported; `>= cursor` → difference; `< cursor` → counter restarted, whole report is new), (b) `INSERT … ON CONFLICT (session_id, turn_seq) DO UPDATE SET <field> = <field> + excluded.<field>, model = COALESCE(excluded.model, model)` into `usage_delta` (the INSERT side uses `COALESCE(?3, (SELECT model FROM agent_session WHERE id = ?1))`, `store.rs:443-445`) and replaces `usage_cursor` — only when some delta > 0, (c) grows `agent_session.token_totals` by the delta, replaces the gauge fields when `Some`, sets `updated_at`, resyncs `sent → pending` (`store.rs:407-518,3007-3019`).

**Step 6 — `turn_finished` / `turn_failed`** → `flush_turn`: for each pending message with a non-blank body, `Job::Turn{native_message_id = wire message id, role, mode, body, created_at = message.started_at}`, then `Job::FinishTurn` (`:4085-4089,767-829`). Worker: `record_turn` (redact → `record_message`; `native_message_id` makes a re-run a no-op; an **assistant** body that strips to blank is skipped) then `complete_turn` → `UPDATE turn SET state='completed', ended_at=now WHERE … AND state='open'` and bump `last_activity_at` (`store.rs:818-837`). **Message bodies are therefore written at turn end, with `created_at` = the time the first chunk arrived.**

**Step 7 — `history_rewound{turns}`** → `mark_turns_rewound`: the last `turns` non-rewound turns get `state='rewound', ended_at=COALESCE(ended_at, now)`; rows are kept (`:4091-4099`; `store.rs:842-855`).

**Step 8 — `agent_disconnected`** → `flush_session_end`: pending messages are written but the turn is **left open** (reconciled as `aborted` on the next store open); per-session caches evicted (`:4101-4106,787-813`; `store.rs:2550-2555`).

### 4.4 Deltas that capture ignores

`status`, `tool_call_output_chunk` (**the streamed terminal output is not accumulated by capture; the stored `result` is whatever the terminal `tool_call_upserted` snapshot carries**), `plan_updated`, `mode_changed`, `retry_status`, `model_changed` (the model is read from the session snapshot at each prompt instead), `available_commands`, `elicitation_requested`, `title_updated` (**an agent-supplied title never becomes the capture title**), `config_options_updated`, `compaction`, `compression_saved`, `rate_limits`, `permission_request`, `permission_resolved` (`capture.rs(host):4108`). Also skipped: `message_appended` with `role: user`; messages whose accumulated body is blank; writes of a `failed` call; shell windows older than 60 s; everything when `binding.enabled = 0`.

### 4.5 The delta wire — every variant and field (`crates/atlas-agent-wire/src/delta.rs:21-184`)

Envelope on the `atlas:agents` window event: `{"agent_id": "<uuid>", "session_id": "<acp session id>", "kind": "<snake_case variant>", ...fields}` (`delta.rs:186-193`; contract examples `crates/atlas-agent-wire/tests/contract.rs:355-511`). Variants are `#[serde(rename_all="snake_case", tag="kind")]`.

| `kind` | Fields | Capture use |
|---|---|---|
| `status` | `status: idle\|running\|waiting\|error`, `turn_seq: u64` (0 = untracked) | ignored |
| `message_appended` | `message: Message` | starts accumulation (assistant/system) |
| `text_chunk` | `message_id: string`, `delta: string` | appended |
| `thinking_chunk` | `message_id`, `delta` | appended |
| `tool_call_upserted` | `message_id`, `tool_call: ToolCall` | tool_call/file_touch/agent_edit |
| `tool_call_output_chunk` | `message_id`, `tool_call_id`, `delta` | ignored |
| `plan_updated` | `plan: PlanEntry[]` | ignored |
| `history_rewound` | `turns: u32` | marks turns rewound |
| `mode_changed` | `mode_id: string` | ignored |
| `retry_status` | `attempt: u32`, `max_attempts: u32`, `delay_ms: u64`, `last_error: string` | ignored |
| `model_changed` | `model_id: string` | ignored |
| `available_commands` | `commands: json[]` | ignored |
| `usage_updated` | `usage: Usage` | usage ledger (real split) |
| `elicitation_requested` | `request_id: uuid`, `mode`, `message`, `requested_schema?: json`, `url?: string` | ignored |
| `title_updated` | `title: string` | ignored |
| `config_options_updated` | `config_options: json[]` | ignored |
| `context_usage` | `used: u64`, `size: u64`, `cost: f64`, `currency: string\|null` | gauge only |
| `compaction` | `active: bool` | ignored |
| `compression_saved` | `saved_tokens: u64` | ignored |
| `rate_limits` | `primary?: RateLimitWindow`, `secondary?: RateLimitWindow`, `plan_type?: string` | ignored |
| `permission_request` | `request_id: uuid`, `tool_call: json`, `options: json` | ignored |
| `permission_resolved` | `request_id: uuid` | ignored |
| `turn_finished` | `stop_reason: string`, `turn_seq: u64` | flush + complete turn |
| `turn_failed` | `error: string`, `turn_seq: u64`, `error_kind?: "auth"\|"transient"\|"fatal"\|"process_dead"\|"unknown"` | flush + complete turn |
| `agent_disconnected` | `reason: string` | flush, turn left open |

Payload types (`crates/atlas-agent-wire/src/types.rs`):

- `Message` (`:173-199`): `id, role: user|assistant|system, mode: text|tool|thinking, content: string, thinking: string (omitted if empty), tool_calls: ToolCall[], plan?: PlanEntry[], model?: string, images?: [{mime_type, data}] (omitted if empty), timestamp: rfc3339`.
- `ToolCall` (`:59-87`): `id, tool_name: string, title?: string, kind?: string, status: pending|running|completed|failed, arguments: json, result?: string, locations: json[], raw_output?: json, content_blocks?: ToolContentBlock[]` (omitted if empty).
- `ToolContentBlock` (`:94-110`), tagged `type`: `{"type":"diff","path":string,"oldText"?:string,"newText":string}` and `{"type":"terminal","terminalId":string}`. Extracted from ACP `content` arrays, including `{"type":"content","content":{…}}` wrappers (`:117-163`).
- `Usage` (`:209-226`): `input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, reasoning_tokens: u64; cost: f64; currency?: string`.
- `PlanEntry` (`:165-171`): `content, priority?, status`. `RateLimitWindow` (`:229-236`): `used_percent: u8, window_minutes?: i64, resets_at?: i64`.

Projection conventions a CLI must mirror (`crates/atlas-agent-delta/src/lib.rs:10-47`): one wire message per contiguous run of one kind (text vs thinking); tool calls nest under a synthetic message id; user messages never go on the wire; turn start is a status flip; turn identity is stamped by the host.

### 4.6 What the CLI must produce for an equivalent record

Per session: one `agent_session` (+ `binding` singleton once per project, + `workspace_cursor` if it links commits). Per prompt: `turn(open)` + user `agent_message`. Per assistant/thinking run: one `agent_message` at turn end with the wire id as `native_message_id`. Per tool invocation: one `tool_call` (upserted by `native_call_id`) + `file_touch` per written path (with write-time `sha256_after`, `sketch_after`, `existed_before`) + ≤1 `agent_edit`. Per cumulative usage report: a `usage_delta`/`usage_cursor` update. At turn end: `turn(completed, ended_at)`. All text through the redactor first (§5). `counter.seq` must stay monotonic.

### 4.7 Titling (`crates/atlas-checkpoint/src/title.rs`)

Derived, never generated: from the **first user prompt**, already redacted (`title.rs:1-16,34-72`). Order: (1) a slash command as `/name first-arg-line`; (2) first meaningful line of the prose after removing/unwrapping agent markup; (3) first meaningful line of the first `<pasted_content>`; (4) `"Pasted text"`; (5) `"Image"` (`:54-77`). Dropped blocks: `system-reminder, local-command-caveat, local-command-stdout, local-command-stderr, command-message, command-stdout, command-stderr, bash-stdout, bash-stderr, task-notification, user-prompt-submit-hook, ide_opened_file, ide_selection, ide_diagnostics, atlas-memory` (`:81-97`); unwrapped: `bash-input`, `user_query` (`:100`); placeholders `[Image #N]`, `[Pasted text #N …]` removed (`:103-104,184-207`). A line that is only a tag is skipped; markdown prefix chars `# > - * space tab` stripped; whitespace collapsed (`:252-286`). Truncation: 120 chars, cut back to a word boundary if that keeps > 2/3, then `…` (`:288-306`). `set_title_if_absent` only writes when `title IS NULL OR title = ''` (`store.rs:322-332`). The thread store's title and the agent's `title_updated` are separate and do not reach capture.

### 4.8 Host machinery stripped before redaction (`capture.rs(crate):484-521`)

- User bodies: everything from `"═══ Atlas next-steps ═══"` on is cut (`:493,497-500`).
- Assistant/system bodies: a `<next_steps>…</next_steps>` block is removed; an unclosed block is kept verbatim (`:501-519`).
- The importer additionally drops `isSidechain`/`isMeta` lines and envelope prefixes (`import.rs:467-482,811-832`).

### 4.9 Canonical tool name (`crates/atlas-checkpoint/src/tools.rs:91-223`)

1. `arguments.tool_name | toolName | name` matched against the alias table → wins.
2. `arguments.command` is a string → `Bash`.
3. Leading token of `wire_name` then `title` (split on whitespace `( : [ <`), also the tool half of `mcp__<server>__<tool>` or `<server>.<tool>` → alias table.
4. Else by ACP `kind`: `read`→Read; `edit`→Edit if args have `old_string|oldText|old_str|diff|patch`, Write if `content|new_str`, else Edit; `execute`→Bash; `search`, `fetch`, `delete`, `move`, `think`; else `Other`.

Alias table (`:174-193`, case-insensitive): Read ← `read readfile view cat`; Edit ← `edit multiedit str_replace str_replace_editor apply_patch applypatch patch notebookedit`; Write ← `write writefile create create_file createfile`; Bash ← `bash shell exec execute run powershell terminal`; Search ← `grep glob search find find_files list ls codebase_search`; Fetch ← `fetch webfetch websearch web browse`; Delete ← `delete rm remove`; Move ← `move mv rename`; Think ← `think thinking reason`; Task ← `task agent subagent dispatch`. `writes_files()` is true for Edit, Write, Delete, Move (`:86-88`).

### 4.10 Write sampling for edit-shaped calls (`capture.rs(host):831-927`)

Paths come from `extract_paths(locations, diff_paths(content_blocks), arguments)`: all `locations[].path`; else every `diff` block path; else the first of `file_path | path | filePath | target_file | file | notebook_path` (`tools.rs:363-427`). Each is `resolve_path`'d against the root, retrying against the canonicalised root when flagged out-of-repo (`:885-901`). `existed_before`: on a **first, non-terminal** sighting = `fs::exists(root/path)`; on any later or already-terminal sighting = `git ls-tree -r -z --name-only HEAD` membership (cached per `(HEAD mtime, index mtime)`), falling back to `git cat-file -e HEAD:<path>` (`:905-919`; `git.rs:747-832`). Writes are recorded once, at the first terminal sighting (`:922-926`).

### 4.11 Shell window for Bash-shaped calls (`capture.rs(host):226-286,929-1091`)

- Non-terminal first sighting: snapshot `before = git status --porcelain=v1 -z --untracked-files=all` (paths incl. rename origins) and `HeadMark` (`:960-986`; `git.rs:160-193`).
- Terminal sighting: if the window is older than `SHELL_WINDOW_LIMIT = 60 s` → nothing (`:1023-1030`). Else `writes = after − before` with `existed_before` from `tracked_in_head` (`:1031-1049`). Additionally, if HEAD moved (`before_head..after_head`), `git diff --name-status -z -M <from>..<to>` paths are added with `existed_before = kind.existed_in_parent()` and the commits `git rev-list --first-parent --reverse <from>..<to>` are parked as `settled_commits` for the worker (`:1051-1088,270-286`; `git.rs:206-219,357-369`). `Unborn` before-mark diffs from the empty tree `4b825dc642cb6eb9a060e54bf8d69288fbee4904` (`git.rs:278-279`).
- No window (first sighting already terminal): fall back to the **turn anchor** (HEAD at prompt time); the anchor advances after use (`:989-1021`).

### 4.12 Edit patch format (`capture.rs(host):3822-3878`)

`arguments.patch | arguments.diff` verbatim if present; else from `old_string|oldText|old_str` and `new_string|newText|new_str|content` in the arguments; else from the `diff` block for the target path (only a lone block when no target). Synthesised form:

```
--- before
+++ after
-<old line 1>
-<old line 2>
+<new line 1>
+<new line 2>
```

(each line prefixed; halves never spliced across sources). Redacted, then stored in `agent_edit.patch` / spilled to `patch_ref`.

### 4.13 Worker timing constants

`NOTIFY_DEBOUNCE = 250 ms` for `atlas:capture-changed` (`capture.rs(host):370-385`); `IMPORT_SCAN_INTERVAL = 30 s` (`:3261-3266`); drain backoff `30 s` doubling to `15 min` (`:3268-3270`); `SYNC_PASS_BUDGET = 3 s` per Project per drain pass (`:412-414`); HTTP timeout `30 s` (`:2802,3391`).

---

## 5. Redaction — what runs before persistence

- **Every** stored body, tool argument, tool result (text) and edit patch goes through `atlas_redact::redact_auto` (JSON-aware) under `catch_unwind`; the **title** is derived from the flat `atlas_redact::redact` via `scrub()` (`capture.rs(crate):233-235,523-528`; `title.rs:30-31`), which differs from the body only for a JSON-shaped prompt; a panic → `Error::RedactionFailed`, the content is **not written**, and the Session is flagged `needs_attention=1, attention_reason="redaction failed, content was not stored: …"` (`capture.rs(crate):399-472,523-546`; `error.rs:21-22,42-44`). The Session row itself still exists.
- `redact_auto`: trimmed input starting with `{`/`[` that parses as a JSON object/array, or is JSONL (≥2 lines each an object/array), is walked structurally (keys/ids/paths preserved, `url`/`uri` values get only credential-material redaction) and re-serialised **compactly**; anything else takes the flat pass (`crates/atlas-redact/src/lib.rs:216-284,286-349`).
- Six layers, any match redacts: Shannon entropy, ~310 vendored betterleaks rules, provider prefixes (`sk-`, `ghp_`, `xox…`, `sb_secret_`…), credentialed URIs, connection strings, credential key/value (`lib.rs:25-38`; `README.md:26-38`). Placeholder is the literal `[REDACTED]`; redaction is idempotent (`lib.rs:62-65`).
- Counts per category are merged into `agent_session.redaction_counts` (`store.rs:661-692`).
- **Not redacted**: `tool_call.title`, `kind`, `locations`, file paths, binary results (`capture.rs(crate):284-322`).
- Memory-side parity test: `crates/atlas-redact/tests/parity_memory_delta.rs`.

A Go CLI must port an equivalent redactor, including the JSON walk, or Atlas's guarantee that "the local store is never itself a place a secret lives" is broken for CLI-written rows (`lib.rs:3-8` of `atlas-redact`).

---

## 6. Git linkage

### 6.1 Triggers

- The git watcher watches `<gitdir>/HEAD`, `<commondir>/packed-refs`, `<commondir>/refs/` (recursive) and `<gitdir>/index`, debounced 200 ms; each batch enqueues `Job::WalkCommits` (`src-tauri/src/commands/git_watcher.rs:1-19,197-273`). For a linked worktree HEAD/index are in the private gitdir and refs in the common dir, so sibling-worktree commits also fire (`:259-263`).
- An open-time walk runs when the watcher starts (`:280-286`), on `capture_enable` (`capture.rs(host):1252-1256`) and after `refresh_detection`/`git init` (`:1346-1348`).
- A shell call that moved HEAD triggers `link_commits` for exactly those commits (`:3244-3258`).

### 6.2 Walk (`crates/atlas-checkpoint/src/checkpoint.rs:97-189`)

Reconcile rewrites first, then walk, both skipped mid-rebase (`capture.rs(host):3053-3108`). Walk: not a repo or unborn → nothing. `cursor = workspace_cursor.last_seen_commit`; candidates = live Sessions with unconsumed touches. No cursor and no candidates → set cursor to HEAD and stop (`:122-130`). Range: `git rev-list --first-parent --reverse <cursor>..HEAD`; no cursor row but live candidates → `git rev-list --first-parent --reverse --max-count 200 HEAD` with `recovered=0`; a cursor row whose commit no longer resolves → the same scan with `recovered=1` (`:170-189`; `git.rs:357-396`). After each commit the cursor is advanced (`:160-163`).

### 6.3 Per commit (`checkpoint.rs:236-363`)

`git show --no-patch --format=%H%x1f%an%x1f%ae%x1f%P%x1f%ct%x1f%s <sha>` (`git.rs:623-658`). A **merge** creates no Checkpoint; its side commits `git rev-list --no-merges --reverse --max-count 200 <first>..<side>` are evaluated instead (`:260-274`; `git.rs:579-603`). Changed paths: `git show --first-parent --name-status -M --format= -z <sha>` (`git.rs:665-679`). For each candidate Session with `started_at.timestamp() <= commit_time` (`:306-308`): `matching_paths` (case-insensitive, rename old-path aware) → `links()`:

- `Deleted` change → links iff `touch.deleted` (`:431-433`).
- `existed_in_parent && touch.existed_before` → links on path alone (`:435-448`).
- Otherwise strict: `git cat-file blob <sha>:<path>` hash == `sha256_after`, or `git cat-file --filters <sha>:<path>` hash == `sha256_after`, or `sketch::retains_agent_work(touch.sketch_after, sketch(committed)) ` i.e. containment ≥ 0.5 (`:453-497`; `sketch.rs:107-137`).

On ≥1 link: `git show --first-parent --numstat --format= <sha>` for insertions/deletions; `patch_id` = `git patch-id --stable` fed `git diff-tree -p --first-parent --root <sha>` (NULL for an empty diff); `upsert_checkpoint(session, sha, patch_id, branch = current HEAD branch, author name/email, files_touched = linked commit-side paths, stats, sync_state)` keyed on `(session_id, commit_sha)` (`:315-336`; `git.rs:854-913`; `store.rs:2158-2188`). Then **consume** the linked touches: `UPDATE file_touch SET consumed_by_commit=? WHERE session_id=? AND path=? AND consumed_by_commit IS NULL AND created_at <= commit_time+1s` (`:338-361`; `store.rs:2522-2544`).

### 6.4 Rewrites (`checkpoint.rs:554-699`)

Deferred while `rebase-merge | rebase-apply | CHERRY_PICK_HEAD | MERGE_HEAD` exist (resolved via `git rev-parse --git-path …`) (`git.rs:477-523`). Reachability: `git merge-base --is-ancestor <sha> HEAD`, then `git for-each-ref --format=%(refname) --contains <sha>`, then `git cat-file -e <sha>^{commit}` to distinguish "gone" from "probe failed" (`git.rs:319-347`). Unreachable + `patch_id` → candidates from `git rev-list --all --max-count 200` patch-id map; one match → `relink_checkpoint` (absorbing an existing row at the new sha); several → prefer the recorded branch (`git branch --format=%(refname:short) --contains`), else orphan (`:754-786`). `orphan_checkpoint` keeps the row, sets `link_state='orphaned'`, `sent → pending` (`store.rs:2364-2371`). An orphan that becomes reachable again is recovered. ≥5 orphans in one pass is a "mass orphan" surfaced by health (`:550-552`).

### 6.5 Worktrees

Capture is **per launch directory**: the store lives in the directory the agent ran in. `main_worktree`/`scope_root` (`git rev-parse --git-common-dir`, bare detection, `--show-toplevel`) and `worktree_paths` (`git worktree list --porcelain`) exist for **shared memory**, which is per repository, and for memory's reading of every worktree's capture store — not for capture's own keying (`git.rs:221-268`; `docs/architecture.md:255`; `src-tauri/src/commands/memory_capture.rs:19-25,251-275`).

### 6.6 What is read from git and when (complete list of invocations)

All via `git -C <repo> …`, read-only, no hooks, no refs written (`git.rs:1-11,104-144`): `rev-parse --git-dir` / `HEAD` / `-q --verify HEAD` / `--is-shallow-repository` / `--git-common-dir` / `--is-bare-repository` / `--show-toplevel` / `--git-path …`; `symbolic-ref -q HEAD`, `symbolic-ref --quiet --short HEAD`; `rev-list --max-parents=0 HEAD` (root commit, last line); `remote get-url origin` (normalised: scheme/userinfo stripped, scp `git@host:` → `host/`, trailing `/` and `.git` removed, lowercased, `git.rs:441-464`); `status --porcelain=v1 -z --untracked-files=all`; `diff --name-status -z -M a..b`; `rev-list --first-parent --reverse [--max-count N] range`; `rev-list --all --max-count N`; `rev-list --no-merges --reverse --max-count N a..b`; `merge-base --is-ancestor`; `for-each-ref --format=%(refname) --contains`; `cat-file -e`, `cat-file blob`, `cat-file --filters`; `branch --format=%(refname:short) --contains`; `show --no-patch --format=…`, `show --first-parent --name-status -M --format= -z`, `show --first-parent --numstat --format=`; `diff-tree -p --first-parent --root`; `patch-id --stable`; `ls-tree -r -z --name-only HEAD`; `worktree list --porcelain`; and `git init` from the popover (`capture.rs(host):1382-1386`).

Detection at bind time records `root_commit_sha` (advisory), `fingerprint_is_shallow`, `git_url` (`binding.rs:34-47,80-91`).

---

## 7. Backend sync

**Present today, cloud mode only.** Local mode never touches the network (`lib.rs:35-37`).

### 7.1 Endpoints (`crates/atlas-checkpoint/src/sync.rs`)

Base URL: `ATLAS_INGEST_URL` env var → build-time `option_env!("ATLAS_INGEST_URL")` → `https://ingest.tryatlas.cc`; trailing `/` trimmed (`sync.rs:67-85`). Auth: `Authorization: Bearer <access token>` on every call; the token is minted by the desktop auth core (`AuthState.core().mint_access_token()`, session file `atlas-session.json` under the app's private config dir) and refreshed at most once per drain on a 401 (`capture.rs(host):2769-2785`; `src-tauri/src/auth/store.rs:1-44`; `sync.rs:205-210,429-447`). Client: reqwest blocking, rustls, 30 s timeout.

| Call | Request | Response / codes |
|---|---|---|
| `POST {base}/ingest` | `{"artifacts":[AtlasArtifact…]}` ≤100 artifacts, ≤4 MiB | 2xx: `{"results":[{"rowId","accepted":bool,"reason"?}]}` or empty body = whole batch accepted; 401 refresh once; 403 terminal `not_authorized`; 429 honour `Retry-After` (seconds only); 5xx/no response = offline; other 4xx = rejected → bisect to the poison row (`sync.rs:465-520`, `:54-65,330-422`) |
| `PUT {base}/blobs/{sha256}` | raw bytes | 2xx uploaded; 409 already present; 401/403/413/429 as above (`:539-579`) |
| `GET {base}/workspaces/slug-available?orgId=&slug=` | — | `{"available":bool}`; 409 = taken (`:598-641`) |
| `POST {base}/workspaces` | `{"orgId","slug","visibility":"org"\|"restricted","name"?,"rootCommitSha"?,"gitUrl"?}` (absent, never null) | `{"workspaceId"\|"id"}`; 409 slug taken (`:694-740`) |
| `GET {base}/workspaces?orgId=` | — | `{"workspaces":[{"id","slug","rootCommitSha"?,"gitUrl"?,"name"?,"visibility"?}]}` (`:745-759,850-882`) |
| `POST {base}/workspaces/connect` | `{"orgId","create":bool,"workspaceId"?,"slug"?,"rootCommitSha"?,"gitUrl"?}` | `{"status":"connected","workspaceId","slug"?,"reason":"explicit\|fingerprint\|origin_url\|created\|already_connected"}` or `{"status":"ambiguous","candidates":[…]}` else no match (`:925-1041`) |

Error bodies are `{"error":{"code","message"}}` (`:982-1011`).

### 7.2 Artifact payloads (`crates/atlas-checkpoint/src/artifacts.rs`)

Tagged by `"type"` ∈ `agent_session | agent_message | tool_call | checkpoint`; all other keys camelCase; base fields flattened: `rowId, orgId, workspaceId, seq, contentHash, createdAt` (`:22-95`).

```json
{"type":"agent_session","rowId":"as-…","orgId":"…","workspaceId":"<remote id>","seq":0,"contentHash":"…","createdAt":"<started_at>",
 "sessionId":"as-…","source":"acp","nativeSessionId":"…","title":"…","agent":"claude-code","model":"…","tokenTotals":{…},"startedAt":"…"}
{"type":"agent_message","rowId":"am-…",…,"seq":<seq>,"sessionId":"as-…","turnSeq":1,"role":"user","mode":"text","preview":"…","body":"…"|null,"bodyRef":"<sha256>"|null}
{"type":"tool_call","rowId":"tc-…",…,"seq":<seq>,"sessionId":"…","turnSeq":1,"toolName":"Bash","title":"…","kind":"execute","status":"completed","locations":[…],"arguments":"…","argumentsRef":null,"result":"…","resultRef":null,"resultBinary":false}
{"type":"checkpoint","rowId":"cp-…",…,"seq":0,"sessionId":"…","commitSha":"…","patchId":"…","linkState":"linked","branch":"main","gitAuthorName":"…","gitAuthorEmail":"…","filesTouched":["…"],"insertions":12,"deletions":3}
```

`contentHash` formulas (`store.rs:1782-1797,1877-1888,1932-1944`): session = sha256(`"{id}:{title}:{agent}:{model}:{token_totals json}"`); message = its `content_hash`; tool_call = sha256(`"{id}:{status}:{result_ref}:{result}"`); checkpoint = sha256(`"{session_id}:{commit_sha}:{link_state}"`). Server dedupes on `(seq, contentHash)`; a changed row re-sends (`sync.rs:166-170`; `store.rs:2626-2632`).

**Never uploaded**: `file_touch`, `agent_edit`, `turn`, `usage_delta/cursor`, `binding`, `import_progress`, rows of **aborted** turns (`store.rs:1818-1831,1863-1867`), and there is no author field — the server stamps authorship from the token (`artifacts.rs:8-14`).

### 7.3 Ordering and the outbox state machine

Blob-first: every `bodyRef`/`argumentsRef`/`resultRef` is `PUT` before the artifact is `POST`ed (`sync.rs:9-17,239-319`). Batch order: pending Sessions (by `started_at`), then messages (by `seq`), then tool calls (by `seq`), then checkpoints (by `created_at`) (`store.rs:1736-1962`). Per-row `sync_state`: `local` → (promotion) `pending` → `sent` / `failed`; `sent` flips back to `pending` when the row's content changes (`RESYNC_*`); `failed` → `pending` only via the human `capture_retry_failed` (`store.rs:1991-2013`; `capture.rs(host):1424-1446`). Rows with `sync_attempts >= 5` are retired to `failed` at the start of each drain (`sync.rs:64-65,182-187`). Rows whose blob is missing locally or is 413 are failed immediately (`:253-283`).

### 7.4 Cadence

Sync worker tick every 30 s queues an import scan and a backoff-gated drain for every open writer store; human actions (`capture_promote`, `capture_connect`, `capture_switch_project`, `capture_retry_failed`, `capture_activate`) queue a forced drain; each drain pass runs ≤3 s then yields to the next Project; offline/429 back off 30 s → 15 min (`delay = max(retry_after, doubled_backoff)`, `:3648-3654`: `Retry-After` can lengthen but never shorten the wait); `drain_state='not_authorized'` stops draining until re-registration (`capture.rs(host):3317-3439,3457-3553,3646-3662`). Gates: `binding.mode='cloud'`, `org_id` set, `remote_workspace_id` or `slug` set (`:3326-3353`).

### 7.5 Registration / promotion flows

- Create cloud: `capture_enable(local)` → `capture_register_cloud` (server first, then `set_cloud_binding`; clears `import_approved`, resets `drain_state`) (`:2247-2322`; `store.rs:1608-1624`).
- Promote existing local history: `capture_promote` → register → `promote_to_cloud` flips binding **and every `local` row to `pending` in one transaction** (`:2701-2762`; `store.rs:1630-1651`).
- Connect to an existing Project: `capture_connect` → `POST /workspaces/connect` → `promote_to_cloud` (`:2408-2495`). Switch: `switch_cloud_project` requeues every row (`store.rs:1665-1685`).
- The bulk-import disclosure gate: a Cloud Project imports on-disk transcripts only after `capture_import_confirm` sets `binding.import_approved=1`; Local sets it at enable (`:1239-1244,1308-1323,3283-3290`).

The cloud **read** half (remote sessions, comments, WebSocket) is `crates/atlas-artifacts` and is out of scope here (`docs/architecture.md:231`).

---

## 8. The enable flag

- **Primary flag**: `binding.enabled` INTEGER in `<project>/.atlas/sessions.db`, singleton row `id = 1`, **default 1 when the row is created** by `bind` (`schema.rs:572-573`; `store.rs:1561-1597`). `capture_disable` → `UPDATE binding SET enabled=0` (`store.rs:1716-1723`); `capture_enable` re-binds (`enabled=1`) and never demotes a cloud binding (`capture.rs(host):1207-1267`).
- **Existence gate**: a project has *ever* had capture enabled iff `<project>/.atlas/sessions.db` exists (`enabled_on_disk`, `capture.rs(host):2842-2851`). No sessions.db → every reader returns `None`/"off" and nothing is created (`store.rs:72-78`).
- **Pause semantics**: `enabled=0` stops new records; reconciliation and draining of what was already recorded continue (`:3044-3050,3089-3091`).
- There is **no** key in `config.toml` / `atlas_config.rs` for capture; the popover (`src/features/capture/components/capture-popover.tsx:407,1061,1068`) calls `capture_enable`/`capture_disable` directly.
- Related: `binding.import_approved` (import gate), `binding.mode` (`local`/`cloud`).

Atlas recognises a CLI-written project as "capturing" when `sessions.db` exists, migrates to version 10, has `binding(id=1, enabled=1, workspace_id=<canonical path>, root=<canonical path>)`, and the writer lock can be taken.

---

## 9. Complete `<project>/.atlas/` folder map

Legend: **MUST** = the CLI must write it for Atlas to treat the folder as the same captured project; *should* = recommended; — = other subsystem, leave alone.

| Path | Owner | Format / notes | CLI |
|---|---|---|---|
| `.gitignore` | capture | `# Atlas per-project state…\n*\n` (`store.rs:2861-2872`) | *should* (Atlas writes it on first open if missing) |
| `sessions.db` (+`-wal`, `-shm`) | capture | SQLite schema v10, §3 | **MUST** |
| `sessions.lock` | capture | SQLite file used only as an exclusive-transaction lock (`lock.rs`) | **MUST honour** (take `BEGIN EXCLUSIVE` while writing; refuse if busy) |
| `blobs/<hh>/<sha256>` | capture | content-addressed payloads > 64 KiB (`blobs.rs`) | **MUST** when a payload spills |
| `import-resume.json` | capture importer | cache, §3.6 | optional |
| `.write-probe` | capture health | transient empty file (`store.rs:2566-2572`) | — |
| `project.json` | shared memory | `{"projectId":"<12 hex>"}` (`shared_memory.rs:570-581`) | — |
| `memory/` | shared memory + RAG | `memory.sqlite` (record, at the **scope root** = main worktree), `memory.snapshot.sqlite`, `corpus.sqlite`, `corpus.<model>.usearch`, legacy `hnsw.usearch`/`manifest.json`/`docstore.json`, `extracted/` (`crates/atlas-memory/src/record.rs:74`, `health.rs:25`, `corpus.rs:25-65`, `lib.rs:165,575`; `record/legacy.rs:71-77`) | — |
| `memory-index/policy.json` | memory policy | statement-vector cache (`memory_policy.rs:103-106`) | — |
| `shared-memory/` (`events.jsonl`) | legacy shared memory | migrated into `memory/memory.sqlite` (`docs/architecture.md:265`) | — |
| `memory-sharing.json`, `memory-summarizer.json` | memory sharing | `{"enabled":bool}` / summarizer pref (`memory_sharing.rs:65-75`) | — |
| `memory-graph-layout.json`, `knowledge-graph-layout.json` | memory/knowledge graph UI | node positions (`memory_graph.rs:405`; `knowledge_graph_layout.rs:21-23`) | — |
| `code-index/index.db`, `code-index/chunks.<model>.usearch`, `code-index/grep/` | atlas-codeindex / atlas-grepindex | tree-sitter symbol index + vectors + grep snapshot (`crates/atlas-codeindex/src/store.rs:50-54`, `vectors.rs:39`; `crates/atlas-grepindex/src/index.rs:160-164`) | — |
| `knowledge/` (+`covers/`) | knowledge base | markdown notes (`knowledge.rs:30,146`) | — |
| `interactions.jsonl` | knowledge base | `{"type","summary","timestamp"}` per line, append-only (`knowledge.rs:385-391`) | — |
| `editor-state.json` | editor | frontend-owned JSON (`knowledge.rs:416-418`) | — |
| `session.json` | layout/session restore | frontend-owned JSON (`project_session.rs:1-33`) | — |
| `canvas.json`, `canvas-media/` | canvas | `{"version":2,"viewport":{…},"nodes":[],"edges":[]}` + media (`canvas.rs:5-30,56`) | — |
| `plans.json` | plans | `{"plans":[{…,"plan":md,"timestamp"}]}` (`plans.rs:30-60`) | — |
| `pdf-annotations.json` | PDF viewer | `{"byPath":{path:[annotation]}}` (`pdf_annotations.rs:15-45`) | — |
| `recent-files.json` | recent files | array of `RecentFile` (`recent_files.rs:75-95`) | — |
| `logs.jsonl` | activity log | per-project log, capped at 1 MiB (`log.rs:135-141`) | — |
| `repo-meta.json`, `repos/` | GitHub panel | `{repoName: RepoMeta}` + cloned repos (`github.rs:40-41,328`) | — |
| `skills/`, `packs/` (`.pack-projections.json`) | skills/packs | SKILL.md trees; projection ledger (`skills.rs:516,2741`; `atlas-instruction-sync/src/lib.rs:800-803`) | — |
| `screenshots/` | fs commands | PNG captures (`fs.rs:146`) | — |
| `git-status-cache.json` | listed in `docs/architecture.md:278` | no writer found in current `src-tauri/src` | — |

Not in `.atlas/` but relevant: `<app-config-dir>/threads.db` (thread metadata), `<app-config-dir>/agent-transcripts/<cwd-hash>/<session>.json` (Atlas's text transcript: `{id, plugin_id, cwd, created_at, updated_at, messages:[{role, content, timestamp, model?}]}`, `agent_transcript.rs:33-59,268-275,296`), `<app-config-dir>/session-chat/<agent_session_id>/<chat_id>.json` (`session_chat_sessions.rs:1-11,81-94`), `<app-config-dir>/atlas-session.json` (auth, `src-tauri/src/auth/store.rs:44`), `~/.atlas/log/`, `~/.atlas/memory/`, `~/.atlas/memory-repos/` (`log.rs:21-34`; `atlas-memory/src/global.rs:119`; `memory_repo.rs:44`). `<app-config-dir>` = Tauri `app_config_dir()` = `~/Library/Application Support/dev.atlas.ide/` on macOS (`docs/architecture.md:255`).

The importer **reads** `~/.claude/projects/<encode_cwd(cwd)>/*.jsonl` (`capture.rs(host):3718-3722`; `crates/atlas-agent-transcript/src/discovery.rs:26-41`) and parses: `type ∈ user|assistant` lines with `message.content` (string or `[{type:text|thinking|tool_use|tool_result}]`), `uuid`, `timestamp`, `message.model`, `message.usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}` deduped by `requestId`, `isSidechain`, `isMeta` (`import.rs:467-513,749-778,834-988`). Imported Sessions get `agent = "claude-code"` by default, no branch, no Checkpoints, `started_at` backdated to the earliest line timestamp (`import.rs:18-24,536-556,694-699`).

---

## 10. Minimum compatibility checklist for the Go CLI

1. Write `<root>/.atlas/sessions.db` with exactly the V1–V10 DDL and `PRAGMA user_version = 10`; WAL mode; foreign keys on. Do not add columns (the store asserts the migrated shape against a fresh one in tests, `schema.rs:894-1027`).
2. Take `sessions.lock` (`BEGIN EXCLUSIVE`, non-blocking) for the duration of any write session; release by closing. Atlas falls back to read-only while you hold it and resumes when it is released (`capture.rs(host):2865-2878`).
3. `binding`: `id=1`, `workspace_id = root = <canonical absolute path, no trailing slash>`, `mode='local'`, `enabled=1`, `import_approved=1`, `drain_state='ok'`, `root_commit_sha`/`fingerprint_is_shallow`/`git_url` from `git rev-list --max-parents=0 HEAD` (last line) / `rev-parse --is-shallow-repository` / normalised `remote get-url origin`, `created_at`/`updated_at` RFC 3339 `+00:00`.
4. Ids: `as-|am-|tc-|ft-|ae-|cp-` + 32-hex UUID v4. `seq` from `counter('seq')` in the same transaction.
5. Timestamps: UTC RFC 3339 with `+00:00`, from `chrono::to_rfc3339` semantics (fractional seconds optional).
6. `agent_session`: `source` = `acp` or `native` (CLI-run agents are not "native" unless they are the Atlas engine; use `acp`), `native_session_id` = the agent's own session id, `agent` = plugin id string, `cwd` = the launch dir, `branch` = branch at first prompt (first wins), `token_totals` JSON as §3.4, `redaction_counts` JSON, `last_activity_at` maintained monotonic.
7. Prompt → `turn(open)` + user `agent_message` (`native_message_id = prompt-{turn_seq}-{sha256}`), machinery stripped, redacted, title set if absent (§4.7).
8. Assistant/thinking → one `agent_message` per contiguous run, written at turn end with `created_at` = first-chunk time; `mode='thinking'` rows hold the reasoning text.
9. Tool calls → `tool_call` upsert by `native_call_id`, canonical `tool_name` (§4.9), `locations` JSON, redacted `arguments`/`result` (JSON-aware), binary results as blobs with `result_binary=1`.
10. Writes → `file_touch` per path with **write-time** `sha256_after` + `sketch_after` (FNV-1a sketch, §3.4) + `existed_before` + `deleted` + `out_of_repo`, paths project-relative/NFC; ≤1 `agent_edit` per call with the §4.12 patch. Failed calls record no writes.
11. Usage → `usage_delta`/`usage_cursor` per §4.3 step 5; keep `token_totals` cumulative; never put a context gauge into the split fields.
12. Turn end → `turn.state='completed', ended_at`; agent death → leave `open` (Atlas marks `aborted` on next open).
13. Checkpoints are **optional** for the CLI: if you implement linking, follow §6 exactly (first-parent walk, asymmetric rule, consumption with the +1 s bound, `(session_id, commit_sha)` idempotency, `workspace_cursor` advanced per commit). Otherwise leave `workspace_cursor` absent and Atlas's own walk will link your Sessions' unconsumed `file_touch` rows the next time the project is opened in Atlas (`checkpoint.rs:97-102`; `capture.rs(host):688-706`).
14. Redact everything text-shaped with an equivalent six-layer, JSON-aware, idempotent redactor emitting `[REDACTED]`; on failure write nothing and set `needs_attention=1, attention_reason`.
15. For cloud: leave `sync_state='local'` and let the desktop promote, **or** set the binding to cloud yourself only after a successful `POST /workspaces` / `/workspaces/connect`, then write rows as `pending` and drain with the §7 protocol (blob-first, ≤100 artifacts/≤4 MiB per batch, bearer token).
