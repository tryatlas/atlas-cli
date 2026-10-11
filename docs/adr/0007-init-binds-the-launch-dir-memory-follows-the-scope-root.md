# ADR-0007: `atlas init` binds the folder it runs in; memory follows the scope root (O7)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D19 (formerly §10 O7).

## Context

The desktop keys **capture by the launch dir**: `.atlas/sessions.db` lives in
the folder opened as the Project, and `workspace_id` is that folder's
canonical path (evidence-capture.md:37-45, :586). It keys **memory by the
scope root**: `<repo>/.atlas/memory/memory.sqlite`, shared by every worktree
and subdirectory (evidence-memory.md:414). The sharing gate,
`memory-sharing.json`, is per launch dir (evidence-memory.md:424).

The CLI has a problem the desktop does not have. The proxy cannot see which
directory an agent runs in. Inside one `atlas shell` the user may `cd` into a
subdirectory and start `claude` there.

## Decision

1. **`atlas init` is allowed in any directory**, including a subdirectory of a
   repository. When the launch dir is not the scope root, it warns:
   "capture is recorded for <dir> only; shared memory is shared with the whole
   repository at <scope root>". `--yes` skips the prompt. It refuses a
   non-directory.
2. **Project discovery walks up**, like git: from the cwd to the nearest
   ancestor holding `.atlas/sessions.db` with a `binding` row, stopping at the
   scope root (inclusive). Nested bindings resolve to the nearest one. No
   binding found means "not initialised", and the commands that need one say
   to run `atlas init`.
3. **`atlas shell` captures into the Project it was started in.** A session
   started in a subdirectory is recorded in that Project's store with
   `workspace_id` = the Project's canonical path and `cwd` = the agent's real
   directory, taken from hook payloads when present. The desktop then shows it
   under the folder the user opened.
4. **Memory always resolves the scope root** of the session's cwd, as the
   desktop does, whatever the Project.

## Consequences

- One folder opened in the desktop shows every session the CLI captured
  beneath it while the shell ran there. A subdirectory that has its own
  `atlas init` gets its own store, as the desktop would key it.
- Two worktrees of one repo are two Projects for capture and one memory,
  matching the desktop.
- `atlas status` prints both roots so the split is visible.
