CREATE TABLE events (
             seq      INTEGER PRIMARY KEY,
             ts       INTEGER NOT NULL,
             kind     TEXT NOT NULL,
             key      TEXT NOT NULL DEFAULT '',
             agent    TEXT NOT NULL DEFAULT '',
             session  TEXT NOT NULL DEFAULT '',
             payload  TEXT NOT NULL
         );

CREATE TABLE entries (
             id            INTEGER PRIMARY KEY AUTOINCREMENT,
             kind          TEXT NOT NULL,
             key           TEXT NOT NULL DEFAULT '',
             content       TEXT NOT NULL,
             status        TEXT NOT NULL DEFAULT '',
             source        TEXT NOT NULL DEFAULT '',
             agent         TEXT NOT NULL DEFAULT '',
             session       TEXT NOT NULL DEFAULT '',
             confidence    REAL NOT NULL DEFAULT 1.0,
             created_at    INTEGER NOT NULL,
             updated_at    INTEGER NOT NULL,
             last_used_at  INTEGER,
             uses          INTEGER NOT NULL DEFAULT 0,
             content_hash  TEXT NOT NULL,
             seq           INTEGER
         , rev INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'active', scope TEXT NOT NULL DEFAULT 'repo', evidence TEXT NOT NULL DEFAULT '[]', changed_by TEXT NOT NULL DEFAULT '');

CREATE INDEX entries_kind_seq  ON entries(kind, seq);

CREATE INDEX entries_kind_key  ON entries(kind, key);

CREATE INDEX entries_kind_hash ON entries(kind, content_hash);

CREATE TABLE sessions (
             session_id  TEXT PRIMARY KEY,
             agent       TEXT NOT NULL DEFAULT '',
             started_at  INTEGER,
             ended_at    INTEGER
         );

CREATE TABLE legacy_imports (
             source  TEXT PRIMARY KEY,
             at      INTEGER NOT NULL
         );

CREATE TABLE retracted_events (
                 seq  INTEGER PRIMARY KEY
             );

CREATE TABLE forgotten (
                 id       INTEGER PRIMARY KEY,
                 kind     TEXT NOT NULL,
                 session  TEXT NOT NULL DEFAULT '',
                 at       INTEGER NOT NULL
             , seqs TEXT NOT NULL DEFAULT '[]');

CREATE TABLE revisions (
             rev           INTEGER PRIMARY KEY AUTOINCREMENT,
             entry_id      INTEGER NOT NULL,
             op            TEXT NOT NULL,
             kind          TEXT NOT NULL,
             key           TEXT NOT NULL,
             content       TEXT NOT NULL,
             content_hash  TEXT NOT NULL,
             status        TEXT NOT NULL DEFAULT '',
             source        TEXT NOT NULL,
             agent         TEXT NOT NULL,
             session       TEXT NOT NULL,
             confidence    REAL NOT NULL,
             state         TEXT NOT NULL,
             scope         TEXT NOT NULL DEFAULT 'repo',
             evidence      TEXT NOT NULL DEFAULT '[]',
             at            INTEGER NOT NULL,
             chain         BLOB NOT NULL DEFAULT x''
         , note TEXT NOT NULL DEFAULT '');

CREATE INDEX revisions_entry ON revisions(entry_id, rev);

CREATE VIRTUAL TABLE entries_fts USING fts5(
             key, content, content='', contentless_delete=1,
             tokenize='porter unicode61 remove_diacritics 2');

CREATE TABLE embed_cache (
             key    BLOB PRIMARY KEY,
             model  TEXT NOT NULL,
             dims   INTEGER NOT NULL,
             vec    BLOB NOT NULL
         );

CREATE TABLE links (
                 a    INTEGER NOT NULL,
                 b    INTEGER NOT NULL,
                 rel  TEXT NOT NULL,
                 at   INTEGER NOT NULL,
                 by   TEXT NOT NULL,
                 PRIMARY KEY (a, b, rel)
             );

CREATE TABLE episodes (
                 session     TEXT PRIMARY KEY,
                 agent       TEXT NOT NULL,
                 started_at  INTEGER,
                 ended_at    INTEGER NOT NULL,
                 note        TEXT NOT NULL
             );

CREATE TABLE feedback (
                 entry_id  INTEGER NOT NULL,
                 verdict   TEXT NOT NULL,
                 note      TEXT NOT NULL DEFAULT '',
                 by        TEXT NOT NULL,
                 session   TEXT NOT NULL,
                 at        INTEGER NOT NULL
             );

CREATE TABLE dreams (
                 id           INTEGER PRIMARY KEY AUTOINCREMENT,
                 at           INTEGER NOT NULL,
                 model        TEXT NOT NULL,
                 episodes_to  INTEGER NOT NULL,
                 kept         INTEGER NOT NULL,
                 dropped      TEXT NOT NULL DEFAULT '[]'
             );

CREATE TABLE dream_proposals (
                 id        INTEGER PRIMARY KEY AUTOINCREMENT,
                 dream_id  INTEGER NOT NULL,
                 op        TEXT NOT NULL,
                 status    TEXT NOT NULL DEFAULT 'pending',
                 at        INTEGER NOT NULL,
                 revs      TEXT NOT NULL DEFAULT '[]'
             );

CREATE TABLE dream_attempt (
                 id  INTEGER PRIMARY KEY CHECK (id = 1),
                 at  INTEGER NOT NULL
             );

CREATE INDEX revisions_session ON revisions(session, at);
