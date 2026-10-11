# ADR-0001: Shared memory stays single-machine in v1 (O1)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D13 (formerly §10 O1).

## Context

The desktop has no backend for shared memory. The record is
`<scope root>/.atlas/memory/memory.sqlite`, gitignored, local
(evidence-memory.md:5, :408). The only things that leave the machine are the
extractor's model call and file projections a person could copy (AGENTS.md
export, the Agent Memory Repo mirror). The CLI's journey, where agents in one
folder on one server share memory, needs no sync.

Cross-machine sharing would need a server API, a reconcile algorithm that
keeps the blake3 chain verifiable across writers (desktop ADR-0017), an
answer for `purge` (the one op that rewrites history), and org/Project access
rules. None of this exists on the server, and the desktop would have to
implement the same protocol for the record to stay byte-compatible (D11).

## Decision

**v1 ships no cross-machine memory sync.** Same checkout means shared memory;
different machines means separate memory. The CLI does not invent a sync
format, does not commit `memory.sqlite`, and does not write the AGENTS.md
export or the memory-repo mirror (SPEC §5.8).

The one manual bridge (D13, added when the user locked the decision):
`atlas memory export <dir>` writes the active, non-stale durable entries in
the desktop's Agent Memory Repo line format (evidence-memory.md §4.2:
`- <text> [source: …; added: YYYY-MM-DD; atlas-id: <id>; revision: <rev>; code: <path>#L<a>-<b>]`),
and `atlas memory import <file-or-dir>` reads that format back as
**candidates** (confidence 0.3, `source = "import:amr"`, metadata kept as the
revision `note`, deduped by `(kind, content_hash)`), exactly as the desktop's
AMR import does. A person commits the files; nothing syncs by itself.

**Amended 2026-10-10 (Phase 5):** export writes a directory, not one file.
The desktop's AMR import decides an entry's kind from the name of the file
it is in (`decisions`, `architecture`, `failures`, the preferences section of
`MEMORY.md`, else fact; evidence-memory.md §4.2). A single exported file
would come back as all facts. So `export <dir>` writes the mirror layout
(`MEMORY.md` with the index and preferences, `decisions.md`,
`architecture.md`, `facts.md`, `failures.md`), and `import` takes that
directory or any one file in it.

v1 code keeps the door open at no extra cost:

- `revisions` is append-only from the CLI. The CLI never runs `clear` or
  `purge`; `forget` is a tombstone revision.
- `internal/memory/record` exposes `VerifyChain` (SPEC §5.4) as a library
  function and as `atlas memory verify`. A later sync needs it, and it is
  also Phase 5's acceptance test.
- Every revision the CLI writes carries `agent` and `session`, so a later
  log can attribute it.

## Consequences

- A team running agents on two servers does not share memory between them.
  `atlas status` says "memory: this machine only" so nobody assumes otherwise.
- If sync is built, the unit is the `revisions` table keyed by
  `(org, Project, scope-root fingerprint)` (SPEC §1.2 D13). That design starts
  with the desktop and server teams, gets its own ADR, and supersedes this one.
