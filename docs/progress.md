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
