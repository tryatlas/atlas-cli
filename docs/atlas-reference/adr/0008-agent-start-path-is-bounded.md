# ADR-0008: Every hop on an external agent's start path has a deadline, and the user can always get out

**Status:** Accepted (2026-09-12). Working tree of `0.3.2`; not yet released. Plan: `~/.claude/plans/zed-industries-zed-file-users-adib-desk-staged-rocket.md`.

**For agents:** this document is the revert map. The change is four independent layers. If a regression is traced to one layer, revert that layer alone using the section "Reverting" below; do not unwind all four. Each layer's files, constants, and the exact pre-change behaviour are listed so a revert can be done from this page without archaeology.

## Context

Customers on `0.3.2` reported Codex (and sometimes any ACP agent) "taking forever to start", sometimes cured by relaunching the app, and on a fresh signed-DMG install "not starting at all" after installing Codex from the marketplace. A screenshot showed the `Starting Codex` row at 21 minutes with no error and a live composer.

It was not one bug. Every hop on the start path could block indefinitely, none reported, and every recovery route in the renderer was gated behind the thing that was stuck:

1. `LocalRegistryNpxAgent::get_command` ran `npm install <pkg>@0.0.0 - <ver>` on **every** connect with no fetch flags and no deadline. npm's defaults (300 s fetch timeout, two retries) plus Atlas's own retry meant a black-holed registry (captive portal, corporate proxy, DNS stall) cost up to ~30 minutes before Rust saw an error. The npm cache was wiped at every launch, so the packument fetch was always a network hit. On a fresh machine the first send also downloaded Node (~50 MB) and the agent package (~290 MB for codex-acp) under a global mutex with an HTTP client that had no timeouts, and with no progress text because the npx agent type had no `loading_status` channel. The marketplace "install" only writes `installed.json`.
2. `AcpConnection::stdio` raced the connection handle and `initialize` only against the child exiting; `session/new`, `session/load`, `authenticate` and `session/list` had no timeout. The manager evicted a connect entry on error but never on hang, and `restart` was a no-op while `Connecting`. Boot connected every installed agent (`backfill_history`), so the user's click usually joined a future stuck since launch.
3. The renderer's `ensureBound` used a closure-local `pending` flag cleared only in `finally`; a promise that never settled left it `true` for the mount's lifetime, and focus-retry, sign-in retry and the silent retry all early-returned on it. The 30 s stall notice was a one-shot toast that aborted nothing. Stop only reset renderer state. `switchAgentForTab` bailed on status `running`. `agent_disconnected` was routed by session id, which a starting tab does not have.
4. Only the direct `node` child was killed on drop (no process group), so `codex.js` and the native `codex app-server` orphaned. `AgentHost::shutdown` did not release the projector's `Arc<dyn AgentConnection>`. Logs went to stderr only, which a Finder launch discards, so no customer report ever carried the phase that stalled.

Zed (`agent_servers`, `node_runtime`, `agent_connection_store`) has the process-group kill and npm fetch flags Atlas lacked, but shares the no-handshake-timeout and npm-per-connect gaps. This ADR covers both.

## Decision

Four layers, each shippable and revertible on its own.

### Layer 1 — install is bounded, per-version, and visible (`crates/atlas-agent-store`)

- `npm install` runs only when the copy on disk cannot serve the spec: `node_modules/<pkg>/package.json` missing or unparsable, installed version above the ceiling, no executable, or the sidecar `<install_dir>/.atlas-wanted` differs from the current registry spec. A missing sidecar with a satisfied install is adopted (sidecar written, no install) so existing users stay offline; upgrades trigger at the next registry bump.
- npm gets `--no-audit --no-fund --prefer-offline --fetch-timeout 60000 --fetch-retries 2 --fetch-retry-mintimeout 2000 --fetch-retry-maxtimeout 10000`; `blank_user_npmrc` carries `update-notifier=false`. `npm_attempt` has `kill_on_drop` and a 600 s deadline (typed `NpmTimedOut`, not retried).
- The npm cache is created if missing and wiped only inside the fresh-Node-download branch.
- `ReqwestClient` has `connect_timeout(15 s)` and `read_timeout(60 s)`; the Node download is wrapped in a 900 s deadline. A timeout is never cached (the install mutex's `Option` is still set only on success).
- `LocalRegistryNpxAgent` has a `loading_status` sender wired in `store.rs`; it sends "Downloading Node.js…" (only when a download actually happens, via `NodeRuntime::ensure_installed`), "Installing <pkg> <ver>…" (only when npm actually runs), and `None` when the command is ready or failed.

### Layer 2 — every handshake hop terminates (`atlas-agent-servers`, `atlas-agent-manager`, `atlas-acp-thread`, `atlas-agent-delta`, `agent_host.rs`)

- `LoadError::TimedOut { agent, phase, after, stderr }` (thread.rs).
- `connection.rs`: `INITIALIZE_TIMEOUT = 60 s` around the connection-handle select and the initialize select; `REQUEST_TIMEOUT = 120 s` on `session/new`, `session/load`, `session/resume`, `authenticate`, the `session/set_mode` that rides on `session/new`, and `session/list`. `prompt`, `cancel`, `close_session`, `logout` and notifications are untouched. Before the `Exited` error is built, the exit path waits for the stderr reader's EOF signal, bounded by `STDERR_DRAIN_GRACE = 1 s`; the reader normally finishes at once, and the bound only runs out when a descendant still holds the pipe open. (First landed as a flat 100 ms sleep, which under load could build the error before the reader had recorded the line explaining the exit; replaced by the EOF wait in `d3a0cb1b`.)
- `AgentChild` wrapper: `pre_exec(setsid)` on unix and `killpg(SIGKILL)` on drop; `kill_on_drop` kept as backstop. `libc` added to `atlas-agent-servers` under `cfg(unix)`.
- `manager.rs`: `CONNECT_DEADLINE = 20 min` around the whole connect future (mapped to `TimedOut { phase: "connect" }`, so the existing error path evicts the entry); `Connecting` entries carry `started_at`; a restart finding a `Connecting` entry older than `RESTART_STALE_AFTER = 30 s` cancels it and starts fresh (younger ones are still joined). `Deadlines` + `set_deadlines()` are test-facing.
- `DeltaProjector::shutdown()` clears `sessions`/`pending`/`permissions`/`elicitations`; `AgentHost::shutdown` calls it last so the last `Arc<dyn AgentConnection>` drops and the tree is killed at quit.
- `backfill_history` wraps each `import_from` in `BACKFILL_TIMEOUT = 120 s`, does not mark backfilled on timeout, and skips agents whose entry is currently `Error`.
- New command `agents_kill_plugin(plugin_id)` → `AgentHost::kill_plugin` (cancels an in-flight connect or drops a live connection by plugin id). Renderer arg key: `pluginId`.
- `agents.rs` setup: one task forwards `AgentManagerEvent::LoadingStatusChanged` as `atlas:agents` `{kind:"loading_status", plugin_id, status}` and captures `ConnectionFailed` with a `TimedOut` error as telemetry `agent_start_timed_out {agent_id, phase, after_s}`.

### Layer 3 — the renderer can always get out (`src/features/chat`, `src/App.tsx`, `src/types`)

- `lib/with-deadline.ts`: `withDeadline(promise, ms, label)` rejects with `DeadlineError` (`code: "bind-timeout"`). `chat-panel.tsx` wraps both bind hops with `BIND_DEADLINE_MS = 3 min`; a deadline rejection takes the existing failure path (held message → queue chip, status idle, log, report) and additionally calls `agents.killPlugin` + `resetAgent`. `ensureBound` has an epoch and a `bindControlRef` (`retry` / `abandon` / `kick`) so a superseded attempt no longer owns `pending`.
- `loading-state.tsx`: `useElapsed(stallAfterMs)` returns `{ref, stalled}`; `STALL_AFTER_MS = 30 s`. `transcript.tsx` `StallNotice`: "Still starting…" with **Restart agent**, **Switch agent**, **Copy diagnostics**. The `Starting {agent}` label is replaced by the live `agentStartingStatus[pluginId]` when present.
- `handleStop` during `pendingSend` also abandons the bind, kills the plugin and resets the cached agent.
- `switch-agent.ts` `isStartingOnly(sess)`: with a held first message and no session, ⌥/ switches in-tab and carries the message over.
- `chat-store.ts` `failPendingBinds(pluginId, reason?)` + `ChatSession.bindError`; `App.tsx` calls it on `agent_disconnected` keyed by `pluginIdForAgentId`. `agentStartingStatus` store slice + `setAgentStartingStatus`. `src/types/agents.ts` `LoadingStatusEvent`.
- `agents-api.ts`: `agents.killPlugin`, `agents.startDiagnostics`, `resetAgent(pluginId)`, `pluginIdForAgentId`.

### Layer 4 — diagnosable in production (`src-tauri/src/logging.rs`, `src-tauri/src/commands/diagnostics.rs`)

- `logging.rs` adds a daily-rotated, blocking file sink at `~/Library/Logs/dev.atlas.ide/atlas.<date>.log` (7 files kept; `dirs::data_local_dir()/dev.atlas.ide/logs` elsewhere) alongside stderr; `logging::log_dir()` exposes the path. `tracing-appender = "0.2"` added to `src-tauri/Cargo.toml` (already in `Cargo.lock` via another dependency).
- `agents_start_diagnostics(plugin_id) -> String`: install dir state (`package.json`, `.atlas-wanted`, `node_modules` present), managed Node dirs, tail of the newest npm log, tail of the newest Atlas log filtered to agent lines. Backs the **Copy diagnostics** button.

## Consequences

- A start that cannot finish now fails with a message naming the hop (`initialize`, `session/new`, `connect`, `npm install`, Node download) within its deadline instead of parking forever; the error takes the pre-existing bind-failure path in the renderer, so nothing new had to be taught to the queue chip or the sign-in dialog.
- A warm machine's bind no longer touches the network at all. The registry-ceiling semantics are preserved (npm may still resolve below the ceiling when it does install); only *when* npm runs changed.
- `prompt` still has no timeout by design; a wedged agent mid-turn is the existing Stop path's job.
- The stale-restart rule means two users of one agent connection (two tabs) can see one of them cancel a 30 s-old connect the other was waiting on; the waiter gets "the agent was stopped while it was connecting" and its tab re-binds. Accepted: before this, both waited forever.
- Non-unix builds fall back to killing the direct child only.
- Byte-level Node download progress is not shown (`install_archive` has no progress callback); the status text is the plain "Downloading Node.js…".

## Verification

Reproduce the original hang, then confirm each deadline:

- Registry black hole: block `registry.npmjs.org` in `/etc/hosts` (or `npm_config_registry=http://10.255.255.1` in the child env), delete `~/Library/Application Support/dev.atlas.ide/external-agents/registry/npx/codex-acp`, send. Expect an error naming npm within 10 minutes, and with `node_modules` present + sidecar equal, no network at all.
- Fresh machine: delete `…/dev.atlas.ide/node` and `…/external-agents/registry/npx`, install Codex from the marketplace, send. Expect "Downloading Node.js…" then "Installing …".
- Wedged agent: `kill -STOP` the `codex-acp` node child (and separately the native `codex app-server`), send. Expect a `session/new` timeout within 2 minutes and Restart to spawn a fresh tree.
- Orphans: quit Atlas with an agent bound; `ps -axo pid,ppid,command | grep codex` must be empty.
- Prod logs: `bun tauri build --debug`, launch from Finder, `tail -f ~/Library/Logs/dev.atlas.ide/atlas.*.log`.
- Tests: `cargo test` in `crates/atlas-agent-store`, `crates/atlas-agent-servers`, `crates/atlas-agent-manager` (no `[workspace]`, so per crate); `bun run typecheck:app typecheck:test`, `bun run lint`, `bunx vitest run src/features/chat`.

## Reverting

Symptoms that point at a layer, and what to restore. Revert by layer; the layers do not depend on each other except where noted.

| Symptom | Layer | Files |
|---|---|---|
| Agent runs an old package version after a registry bump; "Installing …" never appears; npm errors mentioning `--prefer-offline`/`fetch-timeout`; Node download fails on slow links at exactly 15 min | 1 | `crates/atlas-agent-store/src/{servers.rs,node.rs,http.rs,store.rs}` |
| Connect fails with "did not answer `initialize` within 60s" on a healthy but slow agent; "within 120s" on `session/new`; agents die when Atlas is still open (process-group kill too eager); a tab's connect cancelled by another tab's restart; history backfill missing rows | 2 | `crates/atlas-agent-servers/src/{connection.rs,session_list.rs}`, `crates/atlas-agent-servers/Cargo.toml`, `crates/atlas-agent-manager/src/{manager.rs,lib.rs}`, `crates/atlas-agent-manager/tests/invariants.rs`, `crates/atlas-acp-thread/src/thread.rs`, `crates/atlas-agent-delta/src/projector.rs`, `src-tauri/src/commands/{agent_host.rs,agents.rs}`, `src-tauri/src/lib.rs` |
| Bind fails at 3 min on a legitimately long first install; stall notice shows on a normal start; Stop kills a connection another tab was using; ⌥/ behaves differently while starting; wrong label under the spinner | 3 | `src/features/chat/components/{chat-panel.tsx,transcript.tsx,loading-state.tsx}`, `src/features/chat/lib/{agents-api.ts,switch-agent.ts,with-deadline.ts}`, `src/features/chat/stores/chat-store.ts`, `src/App.tsx`, `src/types/{agent.ts,agents.ts}`, tests `with-deadline.test.ts`, `switch-agent.test.ts`, `chat-store.pending-bind.test.ts` |
| Disk growth in `~/Library/Logs/dev.atlas.ide`; startup slowed by logging; diagnostics command errors | 4 | `src-tauri/src/logging.rs`, `src-tauri/src/commands/diagnostics.rs`, `src-tauri/src/commands/mod.rs`, `src-tauri/src/lib.rs`, `src-tauri/Cargo.toml` |

Per-layer notes for a partial revert:

- **Layer 1 without losing the deadlines:** to go back to "npm install on every connect" keep everything else and only delete the `install_needed` short-circuit in `servers.rs` `get_command` (always call `run_npm_subcommand`). To keep the short-circuit but drop `--prefer-offline`, remove that one flag from `NPM_FETCH_ARGS` in `node.rs`. Tuning knobs: the 600 s npm deadline and 900 s Node deadline are constants in `node.rs`; `connect_timeout`/`read_timeout` in `http.rs`.
- **Layer 2 partially:** each deadline is a constant (`INITIALIZE_TIMEOUT`, `REQUEST_TIMEOUT`, `STDERR_DRAIN_GRACE` in `connection.rs`; `CONNECT_DEADLINE`, `RESTART_STALE_AFTER` in `manager.rs`; `BACKFILL_TIMEOUT` in `agent_host.rs`). Raising one is a one-line change; removing the process-group kill means deleting the `pre_exec(setsid)` + `killpg` in `AgentChild` and letting `kill_on_drop` stand. Layer 3's Restart/Stop depend on `agents_kill_plugin` existing; if the command is removed, also remove `agents.killPlugin` call sites in `chat-panel.tsx` or they will reject.
- **Layer 3 partially:** removing only the 3-minute client deadline means deleting the two `withDeadline(...)` wraps in `ensureBound`; the Rust deadlines from layer 2 still bound the wait at 20 min. The stall notice is gated by `STALL_AFTER_MS` in `loading-state.tsx`. Layer 3's `loading_status` rendering is inert without layer 2's forwarding task in `agents.rs`; that is a safe combination (label falls back to "Starting {agent}").
- **Layer 4:** `logging.rs` can be restored to the stderr-only subscriber (the `file_layer` block is self-contained). `diagnostics.rs` is only reachable from the **Copy diagnostics** button; removing the command requires removing that button's handler in `chat-panel.tsx` (`handleCopyDiagnostics`) and the `startDiagnostics` wrapper.

The `git diff` at the time of writing spans 28 modified files plus five new ones (listed above); `git log` after release will carry the same file set under the commit that landed this ADR.
