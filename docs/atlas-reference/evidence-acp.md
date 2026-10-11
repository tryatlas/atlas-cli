# How Atlas discovers, installs, launches and talks to external ACP agents

Reference for a Go CLI (`atlas run <agent>`) that must behave like the desktop app
without reusing its Rust. Every claim cites `path:line` in this repo as of
`feat/commit-tab-history` (2026-10-10). "Schema" means the ACP wire schema the
Rust SDK pins; JSON shapes are given in the schema's camelCase.

## 0. The SDK and the schema Atlas codes against

| fact | value | source |
|---|---|---|
| ACP SDK crate | `agent-client-protocol = "=2.0.0"`, features `["unstable"]` | `crates/atlas-agent-servers/Cargo.toml:9`, `crates/atlas-acp-thread/Cargo.toml:21`, `crates/atlas-agent-manager/Cargo.toml:11` |
| schema crate pulled in by it | `agent-client-protocol-schema 1.5.0` | `Cargo.lock:153-155` |
| SDK repo | `https://github.com/agentclientprotocol/rust-sdk` (authors `Zed <hi@zed.dev>`, Apache-2.0) | `~/.cargo/registry/src/*/agent-client-protocol-2.0.0/Cargo.toml` (`repository`) |
| schema/spec repo | `https://github.com/agentclientprotocol/agent-client-protocol` | `~/.cargo/registry/src/*/agent-client-protocol-schema-1.5.0/Cargo.toml:29,44` |
| wire protocol version Atlas sends | `ProtocolVersion::V1` (integer `1` on the wire) | `crates/atlas-agent-servers/src/connection.rs:308` |
| minimum accepted from the agent | `V1`; anything lower → `LoadError::Unsupported` | `connection.rs:38`, `:341-345` |
| transport | JSON-RPC, newline-delimited, over the child's stdin/stdout (`Lines` codec) | `connection.rs:212-230` |
| `unstable` features relied on | typed `AuthMethod::Terminal` / `EnvVar` variants, `auth` client capability, `session.configOptions` capability, `session/resume`, `session/list`, `session/delete`, `session/close`, elicitation | `connection.rs:836-861`, `src-tauri/src/commands/agent_host.rs:3128-3131` |

**Go SDK:** unverified. Nothing in this repo or in the vendored Rust SDK (`README.md` lists
only Rust crates: `agent-client-protocol`, `-http`, `-cookbook`) references Go bindings. The
org `github.com/agentclientprotocol` is where any official one would live; a Go CLI should
check there and otherwise implement JSON-RPC over stdio by hand (the surface Atlas uses is
small, see §3).

**Everything is capability-gated, never identity-gated** (ADR-0002, `docs/adr/0002-no-default-acp-agents.md:9`; CONTEXT.md:18 "No ACP agent gets special treatment"). The only per-agent-id code is the launch-env workarounds in §2.4.

---

## 1. The registry, installs, and the managed Node runtime

### 1.1 Registry fetch

| fact | value | source |
|---|---|---|
| URL | `https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json` | `crates/atlas-agent-store/src/registry.rs:31-32` (`REGISTRY_URL`) |
| fetch timeout (request + body) | 30 s | `registry.rs:36` |
| icon fetch timeout | 10 s | `registry.rs:37` |
| refresh throttle | at most once per **60 min** (`refresh_if_stale`) | `registry.rs:33`, `:258-269` |
| when refreshed | on install-map change that names a registry agent (`AgentServerStore::set_settings`, `store.rs:141-145`), on marketplace open/refresh (`src-tauri/src/commands/registry.rs:175-182`, `src-tauri/src/commands/catalog.rs:337-351`), on install of an id not in cache (`commands/registry.rs:265-270`) |
| cache location | `<app_data_dir>/external-agents/registry/registry.json`, icons at `.../registry/icons/<sanitized id>.svg` | `crates/atlas-agent-store/src/lib.rs:170-177`, `registry.rs:533-535`, `:440-443`; written atomically (`registry.rs:644-662`) |
| cache semantics | cache-first; a failed refresh keeps the previous catalogue and records `fetch_error` | `registry.rs:1-16`, `:236-253` |
| `<app_data_dir>` | Tauri `app_data_dir()` — macOS `~/Library/Application Support/dev.atlas.ide/` (the dev profile uses `dev.atlas.ide.dev`) | `src-tauri/src/commands/agents.rs:727-730`; `docs/architecture.md` §Persistence |
| HTTP client | reqwest, `connect_timeout 15 s`, `read_timeout 60 s`, User-Agent `atlas/<version>` | `http.rs:84-85`, `agents.rs:731-734` |
| non-2xx | error (any non-2xx, not only 4xx) | `registry.rs:312-318` |
| icon URL rule | absolute `http(s)://` as-is, else `https://raw.githubusercontent.com/agentclientprotocol/registry/main/<id>/<icon minus "./">` | `registry.rs:481-492` |

### 1.2 Registry JSON shape (as parsed)

Top level is strict; **elements are parsed one at a time and an entry that fails to parse is dropped with a log, not fatal** (`registry.rs:563-592`, deliberate divergence from Zed).

```jsonc
{
  "version": "<string>",               // read, unused
  "agents": [
    {
      "id": "codex",                   // required, becomes AgentId
      "name": "Codex",                 // required
      "version": "0.81.2",             // required (string; semver-ish, see is_upgrade)
      "description": "…",              // required
      "repository": "https://…",       // optional
      "website": "https://…",          // optional
      "icon": "./icon.svg" | "https://…", // optional
      "distribution": {                // required
        "binary": {                    // optional map platform-key -> target
          "darwin-aarch64": {
            "archive": "https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>.tar.gz", // required
            "cmd": "./bin/agent" | "agent.exe" | "node",   // required
            "args": ["--acp"],         // default []
            "sha256": "<hex>",         // optional
            "env": { "K": "V" }        // default {}
          }
        },
        "npx": {                       // optional
          "package": "@zed-industries/codex-acp@0.81.2", // required (name@version)
          "args": [],                  // default []
          "env": {}                    // default {}
        }
      }
    }
  ]
}
```
Source: `registry.rs:595-636` (`RegistryEntry`, `RegistryDistribution`, `RegistryBinaryTarget`, `RegistryNpxDistribution`).

**Platform keys** (`registry.rs:503-531`): `darwin-aarch64`, `darwin-x86_64`, `linux-aarch64`, `linux-x86_64`, `windows-aarch64`, `windows-x86_64`; anything else → `None` (no platform).

**Binary vs npx selection** (`registry.rs:400-413`): if both exist, binary wins **only** when it has a target for the current platform key; otherwise npx. An entry with neither is skipped. An npx agent "supports every platform" (`registry.rs:117-124`).

### 1.3 The installed map (the only source of runnable agents)

An external agent exists **iff** it has an entry in the installed map (ADR-0002; `crates/atlas-agent-store/src/lib.rs:21-42`; `store.rs:1-12`).

| fact | value | source |
|---|---|---|
| path | `<app_data_dir>/external-agents/installed.json` | `src-tauri/src/commands/agent_host.rs:3335-3337` |
| missing/unreadable | treated as empty map | `agent_host.rs:3341-3354` |
| written by | the registry commands only (`install`, `install_detected`, `uninstall`), disk first then store rebuild | `commands/registry.rs:415-425` |

Shape (Zed's `agent_servers` JSON verbatim; `settings.rs:7-12`, enum at `:24-61`):

```jsonc
{
  "my-agent": { "type": "custom", "command": "~/bin/agent", "args": ["--acp"],
                "env": {"K":"V"}, "default_mode": "…",
                "default_config_options": {}, "favorite_config_option_values": {} },
  "codex":    { "type": "registry", "env": {"SOME_TOKEN":"…"}, "default_mode": "…",
                "default_config_options": {}, "favorite_config_option_values": {} }
}
```
`"extension"` is accepted as an alias of `"registry"` (`settings.rs:50`). `~/` in a custom `command` is expanded (`settings.rs:132-144`, `:185-196`). Empty `args`/`env`/options are omitted on write.

**Install** = write one `{"type":"registry"}` entry; nothing is downloaded until the first connect (`commands/registry.rs:240-284`). **Install detected** = write a `{"type":"custom","command":<absolute path found on PATH>,"args":<registry target args>}` entry (`commands/registry.rs:301-330`, `detection.rs:49-51`). **Uninstall** = remove the entry, drop any live connection; `purge_cache` also deletes the payload dir (`commands/registry.rs:347-396`).

**Detection** (`detection.rs`): for each registry agent with a **binary** target for this platform whose `cmd` is not `"node"`, take the basename of `cmd` and search `PATH` in order (`detection.rs:95-110`, `:116-128`); npx-only agents are never detected (`:89-93`). Windows tries `.exe/.cmd/.bat` (`:138-146`). A detection is an *affordance*, never a spawn rung (ADR-0002).

### 1.4 Resolving an installed entry to a command on disk

Three resolvers, no fallback between them, no PATH search (`servers.rs:1-18`):

**(a) Custom** (`servers.rs:103-133`): path/args as written; env layered `project < (none) < extra < BYOK < entry.env`; `extra_args` appended.

**(b) Registry binary** (`servers.rs:137-265`):
- install dir: `<app_data_dir>/external-agents/registry/<sanitized id>/` (`store.rs:464-465`)
- versioned subdir: `v_<sanitized version>_<sha256(version)[..16]>_<sha256(archive_url [+ "\0sha256:" + lowercase(sha256)])[..16]>` (`archive.rs:239-265`)
- download if that dir is absent; checksum = registry `sha256`, else GitHub's recorded release-asset digest when the URL is `https://github.com/<o>/<r>/releases/download/…`, else **unverified** (`servers.rs:321-330`, `archive.rs:135-147`); staged to a temp file, extracted, renamed into place (`archive.rs:396-406`)
- archive kind by URL suffix (`archive.rs:60-77`): accepted `.zip`, `.tar.gz`/`.tgz`, `.tar.bz2`/`.tbz2`; refused `.dmg .pkg .deb .rpm .msi .appimage .tar.xz .txz .tar .gz .bz2 .xz .7z`; **any other suffix is installed as a raw binary** (`archive.rs:85-100`)
- limits: 2 GiB download, 4 GiB expanded, 100 000 zip entries; every extracted file masked to `0o755` (`archive.rs:378-394`)
- `cmd` resolution (`servers.rs:373-409`): `"node"` → the managed Node binary; otherwise must be `./relative`, `.\relative`, or a bare file name (no `/`, `\`, `:`, `..`) inside the versioned dir and must exist
- final command: `path=<resolved cmd>`, `args = target.args ++ extra_args`, `env = project < target.env < extra < BYOK < entry.env`
- old version dirs GC'd in the background (`servers.rs:226-242`)

**(c) Registry npx** (`servers.rs:413-546`):
- install dir: `<app_data_dir>/external-agents/registry/npx/<sanitized id>/` (`servers.rs:981-983`), canonicalised and de-verbatim'd on Windows (`:1034-1039`)
- ensure managed Node (§1.5), then `npm install` only when needed (`install_needed`, `servers.rs:915-977`): package.json missing, installed version above the ceiling, no `bin`, platform optional package missing/inert in `node_modules/.package-lock.json`, or the sidecar `<install_dir>/.atlas-wanted` differs from the registry spec (`:830`)
- the npm spec is a **ceiling**, not a pin: `pkg@1.2.3` → `pkg@0.0.0 - 1.2.3` (`node.rs:608-620`)
- install = wipe `node_modules` + `package-lock.json`, `npm install <spec> --save-exact --prefer-online` (fallback `--prefer-offline` when online fails), verify platform package, retry once (`servers.rs:737-804`, `:650-718`)
- executable = `node_modules/<pkg>/package.json` `bin` (string, or the entry named after the unscoped package when several) (`node.rs:548-587`)
- final command: `path=<node binary>`, `args = [<executable path>] ++ npx.args ++ extra_args`, `env = (project ∪ npm_command_env) < npx.env < extra < BYOK < entry.env` (`servers.rs:517-534`)

Env precedence everywhere: `project < distribution < extra < BYOK < settings` (`lib.rs:121-136`, `servers.rs:76-93`). "project" is Atlas's own process env (`InheritedProjectEnvironment`, `servers.rs:56-67`), which has already been enriched by §2.2.

### 1.5 The managed Node runtime

| fact | value | source |
|---|---|---|
| version | `v24.11.0` | `node.rs:31` |
| location | `<app_data_dir>/node/node-v24.11.0-<os>-<arch>/` | `node.rs:102-108`, `:240` |
| download URL | `https://nodejs.org/dist/v24.11.0/node-v24.11.0-<os>-<arch>.tar.gz` (`.zip` on Windows) | `node.rs:248-250` |
| os/arch names | `darwin|linux|win` × `x64|arm64` (Node's names) | `node.rs:373-386` |
| checksum | **required**: `https://nodejs.org/dist/v24.11.0/SHASUMS256.txt`, 30 s fetch; mismatch = not installed | `node.rs:689-703`, `:63` |
| download deadline | 900 s, under the install mutex | `node.rs:58`, `:267-279` |
| liveness check | runs `node bin/npm --version` with pinned `--cache/--userconfig/--globalconfig` | `node.rs:332-371` |
| binaries | `bin/node`, `bin/npm` (Windows `node.exe`, `node_modules/npm/bin/npm-cli.js`) | `node.rs:74-84` |
| npm invocation | `node <npm> [--prefix <dir>] <sub> --cache=<node_dir>/cache --userconfig <node_dir>/blank_user_npmrc --globalconfig <node_dir>/blank_global_npmrc --no-audit --no-fund --fetch-timeout 300000 --fetch-retries 2 --fetch-retry-mintimeout 2000 --fetch-retry-maxtimeout 10000 <args…>` | `node.rs:427-466` |
| user npmrc | `update-notifier=false` | `node.rs:68` |
| env stripped from npm | every `npm_config_*` (case-insensitive) and `NODE_ENV` | `node.rs:404-413` |
| npm deadlines | `install` 1800 s; other subcommands 600 s; one retry on spawn/IO failure, never on timeout | `node.rs:38-53`, `:137-167` |
| PATH enrichment for the agent | `npm_command_env`: `PATH = <node bin dir>:<existing PATH>`, plus `NODE_EXTRA_CA_CERTS` pass-through; on Windows also `SYSTEMROOT`, `ComSpec` | `node.rs:471-509` |
| no system Node fallback | decided (§D12-8) | `node.rs:8-16` |

### 1.6 Update behaviour

A registry refresh that moves an installed agent's version **forward** (semver compare; `v` prefix and `2`/`2.0` padding tolerated; non-semver → any change) fires `new_version_available` (`store.rs:407-426`, `:525-565`). Background `prefetch_updates` installs for agents not currently running (`store.rs:220-258`); a running agent is restarted after its turn (`commands/registry.rs:199-240`). Install progress is a watch channel: `"Downloading Node.js…"`, `"Installing <pkg> <ver>…"`, `None` when ready/failed (`node.rs:252-254`, `servers.rs:588-591`, `:538-544`).

### 1.7 Platform matrix (what runs where)

- macOS arm64/x64, Linux arm64/x64, Windows arm64/x64 are the only registry platform keys (`registry.rs:503-531`) and the only Node platforms (`node.rs:373-386`).
- Process-group kill (`setsid` + `killpg`) is Unix-only; Windows kills the direct child only (`connection.rs:613-680`).
- Login-shell PATH enrichment is Unix-only (`host_env.rs:49-66`).
- Windows: `\\?\` verbatim paths are stripped before reaching npm/Node (`node.rs:529-544`); detection searches `.exe/.cmd/.bat`.
- CI tests only aarch64 (`CLAUDE.md`).

---

## 2. Launch

### 2.1 The process spawn (`AcpConnection::stdio`, `connection.rs:170-407`)

```
command.path  = <resolved path from §1.4>
args          = <resolved args>                        // connection.rs:179
stdin/stdout/stderr = piped                            // :180-182
kill_on_drop  = true                                   // :184
env           = command.env (full replacement map built by §1.4; applied with .envs(), i.e. on top of the inherited env)  // :185-187
cwd           = ConnectOptions.root_dir if Some          // :188-190  — Atlas passes None (agent_host.rs:538), so cwd = Atlas's own cwd
pre_exec      = setsid() on unix (own session/process group)  // :636-646
```
stdout is read line-by-line into the SDK codec; stderr is read line-by-line, logged at `warn`, and kept in a ring buffer for error messages (`:231-244`). Both directions are tee'd to a debug log (`:214-230`).

Process teardown: dropping the child does `killpg(pgid, SIGKILL)` then `start_kill()` (`:659-669`); dropping the connection aborts the wait/io/stderr tasks (`:934-946`). Rationale: `codex-acp` is `node → codex.js app-server → native codex app-server` and killing only `node` orphaned the rest (`:613-625`).

### 2.2 Host env fix-ups before any spawn (`host_env.rs`, called once at boot)

1. `CLAUDECODE` is removed from Atlas's environment (`host_env.rs:23-31`): `@zed-industries/claude-code-acp` refuses to start when it is set (anti-nesting guard).
2. PATH enrichment, Unix only, three passes (`:33-66`):
   - prepend `$HOME/.local/bin`, `$HOME/.bun/bin`, `$HOME/.cargo/bin`, `$HOME/.opencode/bin`, `/opt/homebrew/bin`, `/opt/homebrew/sbin`, `/usr/local/bin`, `/usr/local/sbin`, `/usr/bin`, `/bin` (`:118-136`);
   - run `$SHELL -lic "printf '%s' \"$PATH\""` (login **and** interactive, so `.zprofile` and `.zshrc` both apply), 3 s timeout, child killed on expiry, last non-empty line taken, absolute entries prepended (`:73-115`, `:213-246`);
   - prepend every `~/.nvm/versions/node/*/bin`, newest first (`:142-170`).

A CLI launched from a terminal already has the user's PATH; the `CLAUDECODE` strip is still relevant.

### 2.3 Proxy variables (`server.rs:228-252`)

First of `HTTPS_PROXY, https_proxy, HTTP_PROXY, http_proxy` found is re-exported as canonical `HTTPS_PROXY` or `HTTP_PROXY`; `NO_PROXY` (or `no_proxy`) passed through, else `NO_PROXY=localhost,127.0.0.1` is set whenever a proxy is set (so loopback MCP never goes through it).

### 2.4 The only per-agent-id code: launch quirks (`server.rs:245-343`)

Keyed on registry ids `claude-code`, `codex`, `gemini` (`server.rs:304-306`). Explicitly *not* a list of supported agents (`:300-303`).

| id | env/args added | why | source |
|---|---|---|---|
| `claude-code` | `ANTHROPIC_API_KEY=""` (blanked, not unset) | with a key present the CLI bills the key instead of the subscription | `server.rs:321-325` |
| `codex` | pass through `CODEX_API_KEY`, `OPENAI_API_KEY` if set in host env | the CLI reads them from its own env | `:326-335` |
| `gemini` | `SURFACE=atlas` | identifies the host to Gemini telemetry | `:343-345` |
| `claude-code` (when `atlasOnlyMemory`, default on) | `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` | turn the agent's own memory off | `:266-290` |
| `codex` (same setting) | args `-c memories.use_memories=false -c memories.generate_memories=false` | same | `:283-292` |

These are the `extra_env` / `extra_args` handed to `get_command` (`server.rs:180-196`).

### 2.5 Connect sequence and every deadline

Order inside `CustomAgentServer::connect` → `AcpConnection::stdio` (`server.rs:169-200`, `connection.rs:170-407`):

1. resolve command (may download Node / npm install / archive; progress via loading-status channel)
2. spawn (above)
3. race **connection-handle arrival** vs child exit vs `INITIALIZE_TIMEOUT` (`:263-297`)
4. send `initialize`; race vs child exit vs `INITIALIZE_TIMEOUT` (`:299-339`)
5. **the 250 ms grace**: if `initialize` fails (pipe closed) wait up to `INITIALIZE_EXIT_GRACE` for the exit status, because the exit status carries the stderr that explains the death; report `LoadError::Exited{status, stderr}` if it lands, else the RPC error (`:46`, `:316-326`)
6. on exit after this point: `LoadError::Exited` is fanned out to every live thread (`:350-361`); before the error is built the stderr reader's EOF is awaited up to `STDERR_DRAIN_GRACE` (`:711-732`)
7. reject `protocolVersion < 1` (`:341-345`)

| deadline | value | applies to | source |
|---|---|---|---|
| `INITIALIZE_TIMEOUT` | 60 s | connection handle; `initialize` | `connection.rs:57` |
| `INITIALIZE_EXIT_GRACE` | 250 ms | prefer exit status over RPC error | `:46` |
| `STDERR_DRAIN_GRACE` | 1 s | stderr EOF before building `Exited` | `:62` |
| `REQUEST_TIMEOUT` | 120 s | `session/new`, `session/load`, `session/resume`, `authenticate`, `session/set_mode` riding on new, `session/list` | `:71`, `:697-706`, `:966-975`, `:1024`, `:1073`, `:1195`, `:1430`; `session_list.rs:81-89` |
| `session/prompt` | **no timeout** | — | `:73-74` |
| `CANCEL_GRACE` | 5 s after `session/cancel` before the turn is resolved locally as `cancelled` | prompt | `:86`, `:1243-1275` |
| `CONNECT_DEADLINE` | 20 min on the whole connect future (download included) | manager | `manager.rs:30` |
| `RESTART_STALE_AFTER` | 30 s: a restart joins a younger in-flight connect, replaces an older one | manager | `manager.rs:41` |
| `BACKFILL_TIMEOUT` | 120 s per agent, boot history backfill | host | `agent_host.rs:292` |
| `SYNC_AGENT_TIMEOUT` / `SYNC_DEBOUNCE` | 20 s / 30 s, project sync | host | `agent_host.rs:298`, `:305` |
| npm install / other npm / Node download / SHASUMS | 1800 s / 600 s / 900 s / 30 s | store | `node.rs:38-63` |
| npm per-socket idle | 300 000 ms, 2 retries 2–10 s | store | `node.rs:427-438` |
| HTTP connect / read | 15 s / 60 s | store | `http.rs:84-85` |
| registry / icon fetch | 30 s / 10 s | store | `registry.rs:36-37` |
| login-shell probe | 3 s | host env | `host_env.rs:93-96` |
| renderer bind deadline / stall notice | 3 min / 30 s | UI | ADR-0008 §Layer 3 |

ADR-0008 (`docs/adr/0008-agent-start-path-is-bounded.md`) is the authority and revert map for all of these — except its line 27, which still says `NPM_FETCH_ARGS` carries `--prefer-offline` and `--fetch-timeout 60000`; the code (`node.rs:427-438`) has `300000` and lets each caller pick the cache policy.

### 2.6 Connection lifecycle rules (manager)

- One connect attempt per agent; a second request joins it (`manager.rs:430-478`).
- A failed connect is evicted so the next request retries (`:624-660`).
- A version bump announces; the host restarts after the agent is idle (`watch_new_version`, `:698+`).
- Uninstall drops the connection and forgets its sessions (`handle_agent_servers_updated`, `manager.rs:790-815`; `forget_sessions_for` `:425`).
- `server_for`: native agent always; custom id must be in the catalog, else `Unsupported("… is not installed")` (`manager.rs:541-556`).

---

## 3. The ACP handshake and session protocol, exactly as Atlas does it

### 3.1 `initialize` (`connection.rs:299-306`, `:836-861`)

Request params:
```jsonc
{
  "protocolVersion": 1,
  "clientInfo": { "name": "atlas", "version": "<CARGO_PKG_VERSION>" },   // agent_host.rs:565-566
  "clientCapabilities": {
    "fs": { "readTextFile": true, "writeTextFile": true },
    "terminal": true,
    "auth": { "terminal": true },                       // unstable
    "session": { "configOptions": { "boolean": {} } },  // unstable: we can render boolean config options
    "elicitation": { "form": {}, "url": {} },
    "_meta": { "terminal_output": true, "terminal-auth": true }
  }
}
```
(The exact serialisation is the schema crate's; verify key spellings against schema 1.5.0's JSON schema. The builder calls are at `connection.rs:838-860`.) The `_meta` keys are the pre-stabilisation signals adapters look for: `terminal_output` = "stream `terminal_output`/`terminal_exit` meta on tool-call updates to me", `terminal-auth` = "I can run a `_meta["terminal-auth"]` login spec".

Read from the response (`connection.rs:341-399`):
- `protocolVersion` (must be ≥ 1)
- `agentInfo.name` → telemetry id (falls back to the agent id); `agentInfo.version` → shown version (`:363-370`)
- `authMethods` → stored verbatim (`:395`)
- `agentCapabilities` → stored whole (`:397`); fields actually consulted:
  - `loadSession` (top-level) → `supports_load_session` (`:1004-1008`)
  - `mcpCapabilities.http` / `.sse` → which MCP transports may be sent (`:1010-1012`, `session_mcp.rs:390-401`)
  - `sessionCapabilities.list` → builds the session-list surface; `.delete` → delete allowed (`session_list.rs:52-66`)
  - `sessionCapabilities.resume` → `supports_resume_session` (`:1058-1063`)
  - `sessionCapabilities.close` → `supports_close_session` (`:1112-1114`)
  - `sessionCapabilities.additionalDirectories` → whether extra work dirs are sent (`:460-470`)
  - `auth.logout` → `supports_logout` (`:1210-1212`)
  - `promptCapabilities` → copied onto each thread; `promptCapabilities.image` gates image blocks (`:512`, `prompt.rs:100-119`)

### 3.2 `authenticate` (`connection.rs:1170-1197`, `:902-931`)

- Request: `{"methodId": "<id>"}` (`AuthenticateRequest::new(method)`), under `REQUEST_TIMEOUT`.
- Outcome rules (`authenticate_outcome`): `AuthRequired` error → surfaced as typed `AuthRequired`; `MethodNotFound`, or `InternalError` whose `data.details` contains "not implemented" (case-insensitive) → **`Ok(())` if the method is a terminal method** (the login CLI already ran; the next `session/new` is the real check), else an error "the agent does not support signing in this way"; other `InternalError` with `data.details` → that text as the error.
- A method is "terminal" when it is the typed `{"type":"terminal",…}` variant **or** carries `_meta["terminal-auth"]` (`:1188-1195`).

**Terminal login command** (`terminal_auth_command_for`, `connection.rs:1794-1870`), either shape:
1. typed `{"id","name","type":"terminal","args":[…],"env":{…}}` → run **the agent's own binary**: `command = launched path`, `args = launch args ++ terminal.args`, `env = launch env ∪ terminal.env` (sorted) (`:1800-1822`); id `external-agent-<agent>-<method>-login` (`:1827-1829`).
2. `_meta["terminal-auth"] = {"label","command","args"?,"env"?}` → run exactly that command with exactly that env (`:1834-1870`).
3. neither → `None`; Atlas refuses to guess (no builtin login table).

How Atlas runs it (`agents.rs:2094-2200`): spawn with the spec's env, `stdin=null`, stdout/stderr piped and streamed to the UI as `atlas:auth-run:progress {agentId, runId, stream, line, url}` where `url` is the first `https://` on the line; exit reported as `{success, exit_code, message, agent_id, run_id}`. Interactive logins are handed to a real terminal instead (`:2113-2124`). After the CLI succeeds Atlas calls `authenticate(methodId)` (`agents.rs:1344-1358`) and then rebinds.

`env_var` methods: Atlas never sets them itself; it reports which are satisfied from process/shell env (`agents.rs:1999-2050`), and reads a proprietary `_meta["api-key"].provider` hint (`agent_host.rs:3163-3167`). Auth kind token = raw `type` field: `env_var`, `terminal`, else `agent` (`agent_host.rs:3290-3301`).

`logout` → `LogoutRequest::new()` only when `auth.logout` advertised (`connection.rs:1210-1223`).

### 3.3 `session/new` (`connection.rs:957-1002`, `session.rs:169-213`)

```jsonc
{
  "cwd": "/abs/project",                         // first work dir
  "mcpServers": [ …see §4… ],                    // only transports the agent advertised (admissible)
  "additionalDirectories": ["/abs/other"],      // only if non-empty AND sessionCapabilities.additionalDirectories advertised
  "_meta": { "claudeCode": { "options": { "allowedTools": ["mcp__atlas_memory", "mcp__atlas_code"] } } }  // only when ≥1 HTTP server with no ask-first tools; see §4
}
```
Read from the response: `sessionId`; `modes` (`{currentModeId, availableModes:[{id,name,description}]}`) — or, if absent, synthesised from a `category:"mode"` select among `configOptions` (OpenCode shape; `:1511-1566`); `configOptions` (stored, watched; a `category:"model"` select becomes the model picker, `:1584+`). Then, if the installed entry has `default_mode` and it is one of `availableModes`, `session/set_mode {sessionId, modeId}` is sent under `REQUEST_TIMEOUT` and rolled back locally on failure (`apply_default_mode`, `:1393-1440`).

`agents_new_session` is where "not signed in" usually surfaces: most agents accept `initialize` and reject `session/new` with `AuthRequired` (`agents.rs:1319-1342`); the error class `auth` is what routes the UI into sign-in.

### 3.4 `session/load` and `session/resume` (`connection.rs:1014-1100`, `:526-612`)

Same params as `session/new` plus `"sessionId"`. Two load-bearing rules: the local session/thread is **registered before the RPC resolves**, because `session/load` replays history as `session/update` notifications while the call is in flight (`:526-536`, `:566-577`); and concurrent opens join one in-flight load with a ref count (`:538-546`). The host picks `load` vs `resume` by capability when reopening a history row (ADR-0001 "Resume"; `agent_host.rs:2049-2070`). `session/resume` continues without history (`manager.rs:197-206`).

### 3.5 `session/prompt` (`manager.rs:1030-1092`, `connection.rs:1226-1318`)

Request: `{"sessionId": "...", "prompt": [<ContentBlock>…]}`. Content blocks Atlas builds (`crates/atlas-acp-thread/src/prompt.rs`):
- `{"type":"text","text":"…"}` **always first** (slash commands must be at byte 0 — Claude Code only resolves them there; `prompt.rs:44-49`)
- `{"type":"image","data":"<base64>","mimeType":"image/png"}` for attachments (`:50-58`); stripped when `promptCapabilities.image` is false (`:100-119`)
- `{"type":"resource_link","name":"main.rs","uri":"file:///abs/path"}` for `@`-mentions, after the text (`:77-88`); every agent must accept these
- the text is sent **exactly as typed**; nothing is prepended (ADR-0010)

Response read: `stopReason` (serialised token: `end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, `cancelled`; `project.rs:466-471` serialises rather than hand-formats — ATL-6 regression was `"endturn"`), and `usage` if present (claude-agent-acp fills it, codex-acp does not; `manager.rs:1073-1080`). No timeout on the request.

Error handling in `prompt` (`connection.rs:1283-1316`): `AuthRequired` → typed; `InternalError` with `data.details` containing "This operation was aborted" or "The user aborted a request" **while this turn's cancel fired** → treated as `stopReason: cancelled`; other `InternalError.data.details` → that text as the error; everything else → the error.

### 3.6 `session/cancel` (`connection.rs:1320-1332`, `session.rs:44-85`)

A notification `{"sessionId"}` with no reply. After sending it, a per-session generation counter fires; the in-flight `prompt` waits `CANCEL_GRACE` (5 s) for the agent's own response and otherwise resolves locally as `cancelled` — dropping the SDK request future also sends the JSON-RPC `$/cancel_request` for the abandoned id (`:1258-1272`). Cancel is idempotent (second press = another generation).

### 3.7 `session/request_permission` (agent → client)

Handled by `handle_request_permission` (`handlers.rs:236-285`): the request's `toolCall` update and flat `options` are registered on the thread as `WaitingForConfirmation`; the response is sent when the user answers. Wire to the UI (`crates/atlas-agent-wire/src/delta.rs:154-158`): `PermissionRequest {request_id: uuid, tool_call: json, options: json}`. User answer (`agent_host.rs:171-175`, `:1868-1898`): `{"kind":"selected","option_id":"…"}` → `RequestPermissionResponse{outcome:{outcome:"selected", optionId}}` (the option's `kind` is read back off the pending options, never trusted from the UI); `{"kind":"cancelled"}` → `outcome: "cancelled"` (`thread.rs:409-417`). If the agent cancels the request (`$/cancel_request`), the prompt is taken down (`handlers.rs:272-281`). Selected `AllowOnce|AllowAlways` moves the call to in-progress, `RejectOnce|RejectAlways` to rejected (`thread.rs:421-430`).

### 3.8 `session/update` notification variants Atlas reads

`handle_session_notification` (`handlers.rs:823-937`) then `AcpThread::handle_session_update` (`thread.rs:1205-1310`). Unlisted variants fall through `_ => {}`.

| `sessionUpdate` | fields read | effect |
|---|---|---|
| `user_message_chunk` | `content`, `messageId` | appended unless it echoes the optimistic local user message (`thread.rs:1207-1237`) |
| `agent_message_chunk` | `content`, `messageId` | assistant text (`:1239-1245`) |
| `agent_thought_chunk` | `content`, `messageId` | assistant thinking (`:1246-1252`) |
| `tool_call` | the whole `ToolCall` (`toolCallId`, `title`, `kind`, `status`, `content`, `locations`, `rawInput`, `rawOutput`); plus `_meta.terminal_info.{terminal_id, cwd}` | upsert; `terminal_info` registers a display-only, agent-owned terminal (`handlers.rs:878-897`) |
| `tool_call_update` | `toolCallId` + any fields; plus `_meta.terminal_output.{terminal_id, data}` and `_meta.terminal_exit.{terminal_id, exit_code, signal}` | update; the meta streams output/exit into that terminal (`handlers.rs:905-936`) |
| `plan` | `entries` | plan panel (`:1260`) |
| `session_info_update` | `title` (when defined) | thread title; titles that name Atlas's own prompt machinery are ignored (`:1261-1281`) |
| `available_commands_update` | `availableCommands` | the slash picker (ADR-0005: sourced purely from this) (`:1282-1288`) |
| `current_mode_update` | `currentModeId` | mode pill; also mirrored onto the session's mode state (`handlers.rs:836-845`) |
| `config_option_update` | `configOptions` | replaces the stored options and notifies watchers; a `category:"mode"` select inside it is mirrored to the mode state (`handlers.rs:846-873`) |
| `usage_update` | `size`, `used`, `cost.{amount,currency}` | context usage (`:1297-1308`) |

Unknown `sessionId` → logged and dropped (`handlers.rs:824-832`). Dispatch is a serial queue whose handlers must not block (`handlers.rs:1-33`).

### 3.9 Elicitation (agent → client)

`session/request_elicitation` (`handlers.rs:289-330`): session-scoped go on the thread, request-scoped (no session yet, e.g. device-code login) on the connection. Modes accepted: `form` (with `requestedSchema`) and `url` (http/https with a host only; `elicitation.rs:100-120`). UI event for request-scoped: `atlas:agent-elicitation {agentId, requestId, mode:"url"|"form", message, requestedSchema, url}` (`agents.rs:574-583`, emitted at `:1122-1123`). Answer (`agent_host.rs:254-285`): `{"action":"accept","content":{…}}`, `{"action":"decline"}`, anything else cancels. `session/complete_elicitation` (notification, `elicitationId`) marks a URL elicitation finished out of band (`handlers.rs:337-346`).

### 3.10 Client methods Atlas serves

Registered in `connect_client_future` (`connection.rs:787-826`):

| method | behaviour | source |
|---|---|---|
| `fs/read_text_file` `{sessionId, path, line?, limit?}` | path must resolve inside the session's granted roots (cwd + additionalDirectories) after lexical normalisation and deepest-existing-ancestor canonicalisation (symlink-safe); served from **disk** (not editor buffers); `line` 1-based, `limit` counts lines, streamed; directory → error | `handlers.rs:402-513` |
| `fs/write_text_file` `{sessionId, path, content}` | same containment; creates parent dirs; whole-file write | `:361-400` |
| `terminal/create` `{sessionId, command, args, env, cwd?, outputByteLimit?}` | `DYLD_*`/`LD_*` env dropped; `cwd` must be inside roots; **a permission prompt is raised** (`Run \`cmd args\` in dir`, options `allow_once`/`reject_once`) and only an explicit allow spawns; returns `{terminalId: uuid}` | `:961-1080` |
| `terminal/output` `{sessionId, terminalId}` | `{output, truncated, exitStatus?}` from the buffer | `:1218-1227` |
| `terminal/wait_for_exit` | awaits the process; error for agent-owned (display-only) terminals | `:1229+` |
| `terminal/kill` | kill; no-op for display-only | `:1167-1193` |
| `terminal/release` | kill + forget | `:1194-1217` |
| `session/request_permission` | §3.7 | `:236-285` |
| `session/request_elicitation`, `session/complete_elicitation` | §3.9 | `:289-346` |

### 3.11 Other agent methods Atlas calls

- `session/set_mode {sessionId, modeId}` (`connection.rs:1450-1500`, `:1424-1430`)
- `session/set_config_option {sessionId, configId, value}` where `value` is `{"value": "<valueId>"}` for selects or a boolean form; response's `configOptions` replaces the local list (`:1690-1730`, `:1732-1780`)
- `session/list {cwd?, cursor?}` → `{sessions:[{sessionId, cwd, additionalDirectories, title, updatedAt}], nextCursor}` (`session_list.rs:70-98`, `:137-155`)
- `session/delete {sessionId}` only when `sessionCapabilities.delete` (`:104-121`)
- `session/close {sessionId}` only when `sessionCapabilities.close`, ref-counted (`connection.rs:1113-1143`)
- not used: `session/fork`, rewind/truncate (no ACP equivalent; `agents.rs:1380-1412`)

---

## 4. What is injected into a session: MCP servers, and nothing else

**Only MCP servers.** No skills dir, no instructions, no prompt prefix are injected for an ACP agent: ADR-0005 — `~/.agents/skills` is the canonical store, agents discover their own symlink projections, and the slash picker is sourced purely from `available_commands_update` (`docs/adr/0005-…md:140-160`); ADR-0010 — nothing is prepended to prompts, memory is pulled through the tool server (`docs/adr/0010-…md`, "Decision"). A grep for skills paths in the connect/offer code finds none.

The offer (`src-tauri/src/commands/memory_server/offers.rs:194-317`, `SessionMcpServers` impl):

- One loopback listener bound to `127.0.0.1:0` (`memory_server/host.rs:79`), base URL `http://127.0.0.1:<port>` (`host.rs:118-125`).
- Server names and paths: `atlas_memory` at `/mcp` (`memory_server/mod.rs:57-60`), `atlas_code` at `/code` (`code_server/mod.rs:37-40`), `atlas_ui` at `/ui` (`ui_server/mod.rs:39-42`), `atlas_org` at `/org` (`org_server/mod.rs:97-100`).
- **For an ACP agent only `atlas_memory` and `atlas_code` can be offered**: the request is built with `ui_control: false, org_access: false` (`connection.rs:481-491`).
- `atlas_memory` is included when shared memory is on for the project and the server is running (`offers.rs:55-65`); `atlas_code` by its own setting.
- One bearer token per session, minted **before** `session/new` returns (`tokens.rs:105-126`: two UUIDv4 simple forms concatenated, 64 hex chars), bound to the session id once the agent answers, revoked if the request fails (`offers.rs:293-297`, `session_mcp.rs:1-16`).

**Entry when the agent advertised `mcpCapabilities.http`** (`offers.rs:278-280`):
```json
{ "type": "http", "name": "atlas_memory",
  "url": "http://127.0.0.1:53124/mcp",
  "headers": [ { "name": "Authorization", "value": "Bearer <token>" } ] }
```
**Entry otherwise — the stdio bridge** (ADR-0019; `offers.rs:69-82`):
```json
{ "name": "atlas_memory",
  "command": "/Applications/Atlas.app/Contents/MacOS/atlas",
  "args": [ "mcp-bridge", "http://127.0.0.1:53124/mcp" ],
  "env": [ { "name": "ATLAS_MCP_TOKEN", "value": "<token>" } ] }
```
(`command` is `std::env::current_exe()`; the token is never in argv.) The bridge forwards one JSON-RPC line per POST, replays `initialize`/`notifications/initialized` on a 404'd session, loopback-only, no proxy (`src-tauri/src/commands/memory_bridge.rs:1-60`).

`admissible` (`session_mcp.rs:390-401`) is the gate: stdio always, `http` only with `mcpCapabilities.http`, `sse` only with `.sse`.

**`_meta` pre-approval** (`session_mcp.rs:371-388`): for every offered **HTTP** server with no ask-first tools, `_meta.claudeCode.options.allowedTools` gets `"mcp__<name>"`; the Claude Code adapter passes this to the Agent SDK so Atlas's tools run without a prompt; other adapters ignore it. Omitted when no server qualifies.

The server's `instructions` (MCP `initialize` result) carry the memory protocol (`memory_briefing` first, `memory_search` before asking the user, `memory_changes` on resume, `memory_remember` on decisions) — ADR-0010 table. Session lifecycle: token minted at `session_started`, revoked at `session_ended` (`agents.rs:681-690`, `tokens.rs:202-226`).

---

## 5. The transcript / history model

**Thread identity is Atlas-minted** (ADR-0001; CONTEXT.md:7-9): a *thread* is one conversation keyed by an app-minted `thread_id`; it references at most one agent-side *session* (`session_id`, the ACP id, nullable for drafts). Atlas never reads another program's storage for replay (`docs/adr/0001-…md:51-54`).

**Thread-metadata store**: `<app_config_dir>/threads.db` (SQLite, `crates/atlas-thread-metadata`; `docs/architecture.md` §Persistence — macOS `~/Library/Application Support/dev.atlas.ide/threads.db`). Metadata only, never transcript content (ADR-0001 "The store").

```sql
-- crates/atlas-thread-metadata/src/schema.rs:266-285
CREATE TABLE threads(
  thread_id BLOB PRIMARY KEY, session_id TEXT, agent_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '', title_override TEXT,
  updated_at TEXT NOT NULL, created_at TEXT, interacted_at TEXT,
  folder_paths TEXT, folder_paths_order TEXT,
  main_worktree_paths TEXT, main_worktree_paths_order TEXT,
  remote_connection TEXT, archived INTEGER NOT NULL DEFAULT 0,
  branch TEXT            -- V4, schema.rs:343-345
) STRICT;
CREATE TABLE backfilled_agents(agent_id TEXT PRIMARY KEY, at TEXT NOT NULL);   -- :295-298
CREATE TABLE deleted_sessions(session_id TEXT PRIMARY KEY, at TEXT NOT NULL);  -- :320-323
CREATE TABLE session_aliases(alias_id TEXT PRIMARY KEY, owner_session_id TEXT NOT NULL, at TEXT NOT NULL); -- :333-337
```
`agent_id` here is the plugin/registry id (e.g. `codex`); the native agent's storage key is the literal `atlas-agent` (CONTEXT.md:29). A row is written at bind (`record_connected`, `agent_host.rs:1207-1230`), `interacted_at` on every send (`agents.rs:1774-1780`), the branch on bind and each turn start.

**Atlas's own transcript** (`src-tauri/src/commands/agent_transcript.rs`, state under `<app_config_dir>`): recorded for every agent — the user prompt at send (`agents.rs:1775-1786`), assistant messages/chunks from deltas, flushed at `TurnFinished`/`TurnFailed`/`AgentDisconnected` (`agents.rs:TranscriptMiddleware`). Used for instant first paint (`agents_replay_transcript`, `agents.rs:1446-1473`) and memory handoff; the authoritative replay is still `session/load`.

**Other records** (architecture.md §Persistence): checkpoint/Timeline record `<project>/.atlas/sessions.db`; shared memory `<scope-root>/.atlas/memory/memory.sqlite`.

**Discovery** (ADR-0001 rules 1–7): `session/list` is the floor for any agent that advertises `sessionCapabilities.list`; imported rows land archived (CONTEXT.md:13); rows active in the last 7 days unarchived; deletes are permanent via `deleted_sessions`; a session "live elsewhere" (transcript written in the last ~90 s not explained by an Atlas turn) blocks send without confirmation.

**Delta wire** to consumers (`crates/atlas-agent-wire/src/delta.rs`): one `atlas:agents` event per `SessionDelta` (`MessageAppended`, `TextChunk`, `ToolCallUpserted`, `PlanUpdated`, `PermissionRequest`, `PermissionResolved`, `TurnFinished{stop_reason, turn_seq}`, `TurnFailed{error, turn_seq, error_kind}`, `AgentDisconnected`, `UsageUpdated`, `ContextUsage`, `ModeChanged`, `ModelChanged`, …); additive-only (CLAUDE.md).

---

## 6. Per-agent quirks recorded in comments (observations, not special-casing)

| agent | quirk | where |
|---|---|---|
| any (ATL-6) | stop reason must be the schema token (`end_turn`), not hand-formatted (`"endturn"` bug) | `crates/atlas-agent-delta/src/project.rs:461-471` |
| claude-agent-acp | `authenticate` throws `"Method not implemented."` (SDK wraps as `-32603` with `data.details`) for every method but its gateway ones; for terminal methods that is success | `connection.rs:883-931`, test `:2262-2263` |
| claude-agent-acp | `_meta["terminal-auth"]` spec shape: fully resolved `{label, command, args, env}` | `connection.rs:1885-1911` |
| Claude Code | refuses to start if `CLAUDECODE` is in env | `host_env.rs:12-16` |
| Claude Code | slash commands resolve only at byte 0 of the first text block | `prompt.rs:44-49` |
| Claude Code | `_meta.claudeCode.options.allowedTools` pre-approves MCP servers; `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`; blank `ANTHROPIC_API_KEY` | `session_mcp.rs:371-388`, `server.rs:266-290`, `:321-325` |
| Claude adapter | plan-mode "clear context" continues under a fresh transcript id with no wire signal (alias attribution) | ADR-0001 "Attribution"; `agent_host.rs:403`, `:4459` |
| Claude adapter | replay of a URL-sourced image carries only `uri` | `crates/atlas-agent-delta/src/project.rs:122` |
| claude-agent-acp | `usage` on the prompt response is per-turn and filled; codex-acp leaves it empty | `manager.rs:1073-1074`, `thread.rs:1893` |
| codex-acp | process tree `node → codex.js app-server → codex app-server`; must `killpg` | `connection.rs:613-625`, `:52` |
| codex | reads `CODEX_API_KEY`/`OPENAI_API_KEY`; memories off via `-c memories.*` | `server.rs:326-335`, `:283-292` |
| codex npx | ~220 MB platform tarball; npm exits 0 when the optional platform package fails → tree verified against `.package-lock.json` | `node.rs:40-44`, `servers.rs:720-736` |
| OpenCode | no `modes` on the session; mode arrives only as a `category:"mode"` select in `configOptions`, mode changes as `config_option_update`; models grouped by maker | `connection.rs:1497-1510`, `handlers.rs:846-856`, `:1596-1600` |
| gemini | `SURFACE=atlas` env | `server.rs:343-345` |
| cursor | `_meta["terminal-auth"]` with only `command` ("cursor-agent") | `connection.rs:1914-1927` |
| some agents | report a cancelled turn as `InternalError` "This operation was aborted" / "The user aborted a request" | `connection.rs:1301-1311` |
| some adapters | `session/request_permission` for a tool call never announced via `tool_call` (bare update) | `thread.rs:1652-1654` |
| Windows amp | registry target `cmd` is a bare `amp-acp.exe` rather than `./amp-acp.exe` | `servers.rs:394-409` |
| half the registry | publishes no sha256; GitHub digest or unverified | `lib.rs:92-99` |

---

## 7. Checklist for `atlas run <agent>`

1. Fetch/cached registry (§1.1–1.2); list = `agents[]` filtered to `supports_current_platform`.
2. Install = write `installed.json` entry (§1.3); resolve on first run (§1.4–1.5): managed Node `v24.11.0` with SHASUMS verification, npm ceiling spec, `.atlas-wanted` sidecar, versioned archive dirs.
3. Spawn with piped stdio, own process group, the layered env plus proxy vars and the §2.4 quirks; drop `CLAUDECODE`.
4. `initialize` (§3.1) raced against exit with the 250 ms grace and 60 s timeout; read capabilities/authMethods.
5. If `session/new` returns `AuthRequired`: list `authMethods`, run the terminal login from the typed `terminal` method (agent's own binary + args + env) or `_meta["terminal-auth"]`, then `authenticate`, then retry.
6. `session/new {cwd, mcpServers, additionalDirectories?, _meta?}` (§3.3, §4) — the CLI must host its own MCP server (or the stdio bridge) to hand memory in.
7. Serve `fs/*`, `terminal/*`, `session/request_permission`, `session/request_elicitation` on a non-blocking dispatch queue (§3.10).
8. `session/prompt` with `[text, image…, resource_link…]`, no timeout; `session/cancel` + 5 s grace; consume `session/update` per §3.8; take `stopReason` as the schema token.
9. Persist thread metadata (§5) keyed by an app-minted thread id; replay via `session/load` when `loadSession`, else `session/resume` when `sessionCapabilities.resume`.
