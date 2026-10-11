CREATE TABLE agent_session (
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
    sync_attempts     INTEGER NOT NULL DEFAULT 0, branch TEXT, last_activity_at TEXT,

    -- Identity. Dedupes a re-import *within* a source; deliberately NOT across
    -- sources, because Atlas's ACP-hosted agents also write their own JSONL and
    -- ('acp', id) / ('external_jsonl', id) are both legitimate rows. Skipping
    -- the cross-source duplicate is the importer's explicit job.
    UNIQUE (workspace_id, source, native_session_id)
);

CREATE TABLE agent_message (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    seq               INTEGER NOT NULL,
    turn_seq          INTEGER NOT NULL,
    -- The agent's own id for this message, when it has one. This is what makes
    -- re-processing a turn idempotent rather than duplicating it.
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

CREATE TABLE turn (
    session_id TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq   INTEGER NOT NULL,
    state      TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    PRIMARY KEY (session_id, turn_seq)
);

CREATE TABLE counter (
    name  TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

CREATE INDEX idx_session_outbox
    ON agent_session (workspace_id, sync_state);

CREATE INDEX idx_message_outbox
    ON agent_message (sync_state, seq);

CREATE INDEX idx_message_session_seq
    ON agent_message (session_id, seq);

CREATE INDEX idx_message_facets
    ON agent_message (session_id, role, mode);

CREATE UNIQUE INDEX idx_message_native_id
    ON agent_message (session_id, native_message_id)
    WHERE native_message_id IS NOT NULL;

CREATE INDEX idx_session_started
    ON agent_session (workspace_id, started_at);

CREATE TABLE tool_call (
    id             TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    turn_seq       INTEGER NOT NULL,
    -- The agent's own id for the call — the idempotency key across the
    -- first-sighting event and every later update.
    native_call_id TEXT,
    -- Derived, not handed over: the wire has no canonical tool name. Grouping by
    -- the raw wire value would produce one bucket per file the agent touched.
    tool_name      TEXT NOT NULL,
    -- The human display string, kept separately so the detail view can show what
    -- the agent called it without the facet counts inheriting the churn.
    title          TEXT,
    kind           TEXT,
    status         TEXT NOT NULL,
    locations      TEXT NOT NULL DEFAULT '[]',
    arguments      TEXT,
    arguments_ref  TEXT,
    result         TEXT,
    result_ref     TEXT,
    -- A non-UTF8 result (a compiled binary, an image) is stored verbatim and
    -- skipped by string redaction rather than lossily decoded and corrupted.
    result_binary  INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    sync_state     TEXT NOT NULL DEFAULT 'local',
    sync_attempts  INTEGER NOT NULL DEFAULT 0,

    UNIQUE (session_id, seq)
);

CREATE TABLE file_touch (
    id             TEXT PRIMARY KEY,
    tool_call_id   TEXT NOT NULL REFERENCES tool_call(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq       INTEGER NOT NULL,
    seq            INTEGER NOT NULL,
    -- NFC-normalised, project-relative. macOS hands back NFD while git stores
    -- NFC, and a byte comparison of the two fails silently.
    path           TEXT NOT NULL,
    sha256_after   TEXT,
    existed_before INTEGER NOT NULL,
    deleted        INTEGER NOT NULL DEFAULT 0,
    -- Written outside the Project root. Can never match a commit, and is
    -- flagged so it is not counted as pending agent work forever.
    out_of_repo    INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL
, consumed_by_commit TEXT, sketch_after TEXT);

CREATE TABLE agent_edit (
    id           TEXT PRIMARY KEY,
    tool_call_id TEXT NOT NULL REFERENCES tool_call(id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    turn_seq     INTEGER NOT NULL,
    path         TEXT NOT NULL,
    patch        TEXT,
    patch_ref    TEXT,
    created_at   TEXT NOT NULL
);

CREATE INDEX idx_tool_call_facets
    ON tool_call (session_id, tool_name);

CREATE INDEX idx_tool_call_session_seq
    ON tool_call (session_id, seq);

CREATE INDEX idx_tool_call_outbox
    ON tool_call (sync_state, seq);

CREATE UNIQUE INDEX idx_tool_call_native_id
    ON tool_call (session_id, native_call_id)
    WHERE native_call_id IS NOT NULL;

CREATE INDEX idx_file_touch_path
    ON file_touch (session_id, path);

CREATE INDEX idx_file_touch_by_path
    ON file_touch (path);

CREATE INDEX idx_agent_edit_session
    ON agent_edit (session_id, turn_seq);

CREATE TABLE checkpoint (
    id              TEXT PRIMARY KEY,
    session_id      TEXT NOT NULL REFERENCES agent_session(id) ON DELETE CASCADE,
    -- May change: a rewrite re-points this at the new commit carrying the same
    -- change.
    commit_sha      TEXT NOT NULL,
    -- Stable across rebase and amend, because it hashes the diff rather than the
    -- commit. Null for an empty diff, which never participates in matching.
    patch_id        TEXT,
    link_state      TEXT NOT NULL DEFAULT 'linked',
    -- Empty on a detached HEAD. The timeline's branch filter simply does not
    -- claim those Checkpoints.
    branch          TEXT,
    -- Verbatim from the commit, display-only. This is the identity ON the
    -- commit, which is a different fact from the Atlas account whose agent ran:
    -- pairing, rebasing a colleague's branch and bot commits all diverge. There
    -- is deliberately no mapping from git email to Organisation member — git
    -- emails are self-asserted and never verified, so treating one as proof of
    -- identity would let anyone render commits as a colleague.
    git_author_name  TEXT,
    git_author_email TEXT,
    files_touched   TEXT NOT NULL DEFAULT '[]',
    insertions      INTEGER NOT NULL DEFAULT 0,
    deletions       INTEGER NOT NULL DEFAULT 0,
    -- The agent-versus-human line split. Stays null: this feature captures the
    -- inputs and does not compute the metric, which is a pure function over
    -- stored rows and can be backfilled across every existing Checkpoint later.
    attribution     TEXT,
    created_at      TEXT NOT NULL,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    sync_attempts   INTEGER NOT NULL DEFAULT 0,

    UNIQUE (session_id, commit_sha)
);

CREATE TABLE workspace_cursor (
    workspace_id     TEXT PRIMARY KEY,
    last_seen_commit TEXT,
    -- Set when the cursor could not be resolved and a bounded re-scan was used
    -- instead. Surfaces through the capture-health signal, because a Project
    -- that silently stopped detecting commits is the failure this whole design
    -- exists to avoid.
    recovered        INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT NOT NULL
, reconcile_note TEXT);

CREATE INDEX idx_checkpoint_commit
    ON checkpoint (commit_sha);

CREATE INDEX idx_checkpoint_session
    ON checkpoint (session_id);

CREATE INDEX idx_checkpoint_patch
    ON checkpoint (patch_id);

CREATE INDEX idx_checkpoint_outbox
    ON checkpoint (sync_state);

CREATE TABLE binding (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    workspace_id    TEXT NOT NULL,
    root            TEXT NOT NULL,
    mode            TEXT NOT NULL DEFAULT 'local',
    -- Set once the Project is registered to an Organisation.
    slug            TEXT,
    org_id          TEXT,
    -- Advisory. Null for a non-git Project, or one with no commits yet.
    root_commit_sha TEXT,
    -- The grafted boundary of a shallow clone is not the true root, so the
    -- fingerprint it yields is stored but flagged as not authoritative.
    fingerprint_is_shallow INTEGER NOT NULL DEFAULT 0,
    -- Advisory, normalised. Null when there is no remote — which binds fine.
    git_url         TEXT,
    -- Disabling stops new records without deleting existing ones.
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
, import_approved INTEGER NOT NULL DEFAULT 0, drain_state TEXT NOT NULL DEFAULT 'ok', remote_workspace_id TEXT);

CREATE TABLE import_progress (
    path          TEXT PRIMARY KEY,
    imported_size INTEGER NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_file_touch_unconsumed
    ON file_touch (session_id, path)
    WHERE consumed_by_commit IS NULL;

CREATE INDEX idx_session_activity
    ON agent_session (workspace_id, last_activity_at);

CREATE INDEX idx_message_activity
    ON agent_message (session_id, created_at);

CREATE TABLE usage_delta (
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

CREATE TABLE usage_cursor (
    session_id            TEXT PRIMARY KEY REFERENCES agent_session(id) ON DELETE CASCADE,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_usage_delta_recorded
    ON usage_delta (recorded_at);
