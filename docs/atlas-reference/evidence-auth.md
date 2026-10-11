# Atlas desktop authentication — reimplementation reference for a Go CLI

Source of truth: `src-tauri/src/auth/{mod,core,config,store,backoff,avatar,tests}.rs`,
`src-tauri/src/commands/auth.rs`, and the consumers cited inline. Every claim carries a
`file:line` citation into `/Users/eemong/Codes/atlas`. Where a fact comes from a document
rather than code (the gateway doc, or the research note that quotes the server repo), that
is said explicitly. The server-side auth doc (`atlas-auth-api.md`) is **not in this repo**
(`src-tauri/src/auth/store.rs:28-33`).

Read-only survey, 2026-10-10, branch `feat/commit-tab-history`.

---

## 0. One-paragraph model

The desktop is an OAuth 2.0 **Device Authorization Grant** client (RFC 8628) against a
Better Auth server (`src-tauri/src/auth/mod.rs:1-6`). The thing it stores is the Better
Auth **session token** (opaque, 7-day rolling) returned in the `access_token` field of
`POST /device/token` (`core.rs:81`, `store.rs:200-203`). That session token is sent as
`Authorization: Bearer <session_token>` **only to the auth host**. For every other Atlas
host (AI gateway, ingest, chat) the client first mints a short-lived **access JWT** via
`GET {AUTH}/token` with the session token, then sends `Authorization: Bearer <jwt>`
(`core.rs:1344-1358`; consumers in §7). There is no refresh token: "refresh" means mint
again (`docs/research/agent-org-teams-api-access.md:35-37`).

---

## 1. Base URL resolution

### 1.1 Auth host

| Rung | Source | Citation |
|---|---|---|
| 1 | runtime env `ATLAS_AUTH_URL` (blank/whitespace falls through) | `src-tauri/src/auth/config.rs:24-29`, `:34-38` |
| 2 | compile-time `option_env!("ATLAS_AUTH_URL")` (staging builds) | `config.rs:20-21` |
| 3 | `https://auth.tryatlas.cc/api/auth` | `config.rs:15-18` |

Normalisation: `trim()` then strip **all** trailing `/` (`config.rs:44-46`). The result
already contains the `/api/auth` prefix, and endpoint paths are appended by plain string
concatenation `format!("{base}{path}")` where every `path` starts with `/`
(`core.rs:291-293`). `AuthCore::new` normalises again (`core.rs:264`).

Test oracle (`tests.rs:405-425`):

- `ATLAS_AUTH_URL=http://localhost:8787/api/auth/` → `http://localhost:8787/api/auth`
- `ATLAS_AUTH_URL="   "` with build override `https://staging.example/api/auth` → the build override
- nothing set → `https://auth.tryatlas.cc/api/auth`

**There is deliberately no on-disk override** for the auth host (`config.rs:8-13`): a
file that redirects sign-in is a phishing foothold. A Go CLI should copy this: env var
and build-time only.

**Dev vs prod:** the dev *profile* (bundle id `dev.atlas.ide.dev`) changes only *where
files live* (§4), never the URL (`crates/atlas-profile/src/lib.rs:11-15`). "Dev" URL
pointing is done with `ATLAS_AUTH_URL` (how a developer points at `wrangler dev`,
`config.rs:4`).

### 1.2 The other hosts the JWT is sent to

Not quite the same ladder: `atlas-artifacts` (`lib.rs:85-91`) and `atlas-comms` (`lib.rs:73-80`) neither filter a blank env value nor `trim()`; only `auth/config.rs:36` and `atlas-checkpoint/src/sync.rs:81` fall through on whitespace. `ATLAS_WEB_URL` also has an `option_env!` rung (`crates/atlas-artifacts/src/lib.rs:129-135`).

| Host | Default | Env / build override | Citation |
|---|---|---|---|
| AI gateway | `https://ai.tryatlas.cc/v1` (constant, no env override in code) | — | `crates/atlas-native-agent/src/engine/config.rs:116`; doc `docs/reference/atlas-ai-api.md:78-86` |
| Ingest (Timeline sync + artifacts reads) | `https://ingest.tryatlas.cc` | `ATLAS_INGEST_URL` runtime, then `option_env!` | `crates/atlas-checkpoint/src/sync.rs:75-85`; `crates/atlas-artifacts/src/lib.rs:66-91` |
| Chat | `https://chat.tryatlas.cc` | `ATLAS_CHAT_URL` runtime, then `option_env!` | `crates/atlas-comms/src/lib.rs:68-80` |
| Web app (links only) | `https://app.tryatlas.cc` | `ATLAS_WEB_URL` | `crates/atlas-artifacts/src/lib.rs:121-135` |

The browser approval page is **never constructed** client-side; it comes back in the
device-code response as `verification_uri` (`config.rs:15-17`, `core.rs:36-49`).

---

## 2. Every HTTP endpoint

Conventions shared by all authenticated auth-host calls (`core.rs:783-820`, `:829-865`,
`:872-904`):

- Header: `Authorization: Bearer <session_token>` (reqwest `bearer_auth`).
- JSON bodies via reqwest `.json()` → `Content-Type: application/json`.
- Per-request timeout **10 s** (`AUTHED_TIMEOUT`, `core.rs:22-28`). The unauthenticated
  device endpoints have **no** timeout (`core.rs:622-628`, `:668-677`).
- **No `User-Agent` is set** on auth calls: the client is `reqwest::Client::new()`
  (`commands/auth.rs:44-49`) with no `.user_agent(...)`, so no UA header is sent.
- Status classification, written once (`core.rs:803-819`):

  | Response | Class | Meaning |
  |---|---|---|
  | transport error / timeout / DNS / TLS | `Indeterminate` | learned nothing; retry on backoff |
  | `401` | `Rejected` | **authoritative**; the only class that may clear the credential |
  | `429` or any `5xx` | `Indeterminate { retry_after }` | `Retry-After` read as delta-seconds only (`core.rs:1498-1506`) |
  | any other non-2xx (`400`, `403`, `404`, …) | `Denied` | credential fine, action refused; never retry, never sign out |
  | 2xx | Ok | — |

- `AuthFailure` enum: `NoCredential | Rejected | Denied | Indeterminate{retry_after, reason}`
  (`core.rs:128-152`). `reason` never contains a URL or body (`core.rs:810-814`,
  `redact` at `core.rs:1511-1516` strips reqwest's ` for url …` suffix).
- Rate limit the client is designed around: **100 requests / 60 s window across every
  `/api/auth/*` path, keyed by IP** (`backoff.rs:13-17`; also `core.rs:503-509`).

### 2.1 `POST {AUTH}/device/code` — start the grant

- Code: `core.rs:622-647`. Unauthenticated.
- Request body (exact): `{"client_id":"atlas-desktop"}` (`core.rs:16`, `:625`).
- Response (exact shape the tests serve, `tests.rs:259-270`; struct `core.rs:1518-1526`,
  every field **required**):

  ```json
  {"device_code":"dev-secret","user_code":"ABCD-1234",
   "verification_uri":"https://app.example/device",
   "verification_uri_complete":"https://app.example/device?user_code=ABCD-1234",
   "expires_in":600,"interval":0}
  ```

- Non-2xx → `GrantError::Start("server said <status>")` (`core.rs:630-632`); unparseable
  body → `GrantError::Start("unreadable response: …")` (`core.rs:634-637`); transport
  error → `GrantError::Start(<redacted>)` (`core.rs:626-628`). User message:
  `"Could not reach Atlas: {e}"` (`core.rs:113`).
- `interval` is seconds; the server "normally says 5s" (`core.rs:19-20`).

### 2.2 `POST {AUTH}/device/token` — poll

- Code: `core.rs:667-704`. Unauthenticated.
- Request body (exact, `core.rs:671-675`):

  ```json
  {"grant_type":"urn:ietf:params:oauth:grant-type:device_code",
   "device_code":"<device_code>","client_id":"atlas-desktop"}
  ```

- Success (2xx) body (`tests.rs:374`; struct `core.rs:1528-1531`, only `access_token` read):

  ```json
  {"access_token":"session-tok","token_type":"Bearer"}
  ```

  An empty `access_token` or unparseable 2xx → `Transient` (`core.rs:688-691`).
  **`access_token` here is the Better Auth session token**, not a JWT (`core.rs:81`).
- Error body (non-2xx, non-5xx; tests use `400`, `tests.rs:503`, `:550`, `:576`, `:592`;
  struct `core.rs:1675-1678`):

  ```json
  {"error":"authorization_pending"}
  {"error":"slow_down"}
  {"error":"access_denied"}
  {"error":"expired_token"}
  ```

- Classification (`core.rs:679-703`): transport error → `Transient`; `5xx` → `Transient`;
  2xx+token → `Token`; `authorization_pending` → `Pending`; `slow_down` → `SlowDown`;
  `access_denied` → `Denied`; `expired_token` → `Expired`; any other `error` value or an
  unparseable error body → `Transient`.

### 2.3 `GET {AUTH}/token` — mint an access JWT

- Code: `core.rs:1343-1358`. Bearer = session token.
- Response: `{"token":"<jwt>"}` (`tests.rs:381`; struct `core.rs:1347-1350`).
- Unparseable 2xx → `Indeterminate("unreadable token: …")` (`core.rs:1351-1357`).
- JWT payload the client reads (signature **not** verified, `core.rs:1620-1627`):
  `{"sub":"user_1","orgs":{"org_1":"admin","org_2":"developer"}}` (`tests.rs:389-399`).
  `orgs` is `{ organisationId: role }`; `{}` when the user is in no org
  (`core.rs:1630-1634`). Decoded with base64url, no padding (`core.rs:1647-1652`).
  `exp` is read by the engine's cache (`crates/atlas-native-agent/src/engine/auth.rs:121-134`).
- Documented contract (gateway doc, `docs/reference/atlas-ai-api.md:96`, `:103`,
  `:1076-1077`): audience `"atlas"` (shared with ingest/sync), TTL **10 minutes**,
  re-mint at **T−60 s**. The research note quoting the server repo adds: algorithm
  **EdDSA**, claims `iss, aud="atlas", sub=userId, orgs` (`docs/research/agent-org-teams-api-access.md:37`).
- This is the call whose verdict decides the credential's fate (`core.rs:1360-1400`).

### 2.4 `GET {AUTH}/get-session` — profile ("whoami")

- Code: `core.rs:405-426`. Bearer = session token.
- Response (`tests.rs:290-297`; structs `core.rs:1533-1552`, `:1666-1673`):

  ```json
  {"session":{"id":"sess_1","activeOrganizationId":null},
   "user":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example","image":null}}
  ```

  `session` is `#[serde(default)]`; only `session.activeOrganizationId` is read. `user.image`
  is the provider avatar URL or `null`. Other fields are ignored.
- **A dead session is `200` with body `null` (or empty)**, not a `401` (`core.rs:407-409`,
  `:420-422`). The client treats that as "no profile" and never as a verdict on the
  credential — the verdict comes from `/token` (`core.rs:1362-1367`).

### 2.5 `GET {AUTH}/organization/list`

- Code: `core.rs:501-515`. Bearer = session token.
- Response: JSON array; tests serve `[{"id":"org_1","name":"Atlas","slug":"org_1","logo":null}]`
  (`tests.rs:300-311`); only `id` and `name` are read (`core.rs:1554-1560`). `[]` is a
  real answer ("belongs to none"), stored as such (`core.rs:466-468`).
- This endpoint has names and **no roles**; roles come only from the JWT `orgs` claim
  (`core.rs:428-433`). A failed list (including a `401` here) keeps the previously stored
  list and never clears the credential (`core.rs:435-445`).

### 2.6 `POST {AUTH}/organization/create`

- Code: `core.rs:906-940`. Body: `{"name":"<name>","slug":"<slug>"}` (`core.rs:919-926`).
- Response: `{"id":"<server org id>","name":"<name>"}` (`core.rs:1562-1570`). Creator
  becomes `admin` (`core.rs:909`). A duplicate slug is a `400` → `Denied`
  (`core.rs:914-917`); user message `"Couldn't sync — that organization name or handle may already be taken."` (`core.rs:172-174`).
- Afterwards the client re-pulls profile + list (`core.rs:936-937`).

### 2.7 `POST {AUTH}/organization/check-slug`

- Code: `core.rs:942-971`. Body: `{"slug":"<slug>"}`.
- Response: `200 {"available":true|false}` — the auth worker normalises a taken slug into
  `200 {"available": false}`, so any non-2xx is a real failure (`core.rs:944-947`).
  `available` defaults to `false` if missing (`core.rs:957-961`). Advisory only; create can
  still race (`core.rs:949-951`).

### 2.8 `POST {AUTH}/organization/delete`

- Code: `core.rs:973-1003`. Body: `{"organizationId":"<server org id>"}`
  (`core.rs:983-987`). Admin-only; non-admin gets `403` → `Denied` (`core.rs:975-977`).

### 2.9 `GET {AUTH}/organization/get-full-organization?organizationId=<id>` — members

- Code: `core.rs:1005-1092`; query via reqwest `.query(&[("organizationId", org_id)])`
  (`core.rs:1046-1048`, percent-encoded).
- Response shape read (`core.rs:1012-1040`; all inner fields `#[serde(default)]` except `members[].id`):

  ```json
  {"members":[{"id":"<membership id>","role":"admin","createdAt":"…",
               "user":{"id":"<user id>","name":"…","email":"…","image":"https://…"}}]}
  ```

- `members[].id` is the **membership** id (what `update-member-role` / `remove-member`
  address); `user.id` is the human (`core.rs:1572-1576`). Unknown role string → `null`.
- Runs concurrently with §2.10 to mark the Owner (`core.rs:1042-1050`).

### 2.10 `GET {AUTH}/organization/roster?organizationId=<id>`

- Code: `core.rs:1094-1110`. Only `ownerId` is read (`{"ownerId":"<user id>", …}`);
  any failure → `None` (best effort, never surfaces).

### 2.11 `GET {AUTH}/organization/list-invitations?organizationId=<id>`

- Code: `core.rs:1112-1156`. Response: array of
  `{"id":"…","email":"…","role":"…","status":"pending","expiresAt":"…"}` (all but `id`
  defaulted; missing `status` → `"pending"`). Admin-scoped server-side: non-admin → `403`
  → `Denied` (`core.rs:1114-1115`).

### 2.12 `POST {AUTH}/organization/invite-member`

- Code: `core.rs:1158-1223`. Body (`core.rs:1169-1175`):
  `{"email":"<email>","role":"admin|product_owner|developer|member","organizationId":"<id>"}`.
- Response (`core.rs:1176-1190`):
  `{"id":"…","email":"…","role":"…","status":"pending","expiresAt":"…","acceptUrl":"https://…"}`.
  `acceptUrl` is the same link the server emails the invitee (`core.rs:1160-1162`); only
  this endpoint returns one (`core.rs:1150-1153`).

### 2.13 `POST {AUTH}/organization/cancel-invitation`

- Code: `core.rs:1225-1239`. Body: `{"invitationId":"<id>"}`. Response ignored.

### 2.14 `POST {AUTH}/organization/update-member-role`

- Code: `core.rs:1241-1271`. Body:
  `{"memberId":"<membership id>","role":"<role>","organizationId":"<id>"}`.
  Takes effect in the member's **next** minted JWT (`core.rs:1243-1245`).

### 2.15 `POST {AUTH}/organization/remove-member`

- Code: `core.rs:1273-1301`. Body: `{"memberIdOrEmail":"<membership id or email>","organizationId":"<id>"}`.
  Admin-only. Client re-pulls identity afterwards (you may have removed yourself).

### 2.16 `POST {AUTH}/organization/leave`

- Code: `core.rs:1303-1331`. Body: `{"organizationId":"<id>"}`.
- Success response in tests: `{"id":"mem_1"}` (`tests.rs:2177`).
- Refusal is `403` → `Denied`, e.g. (`tests.rs:2211-2217`):

  ```json
  {"code":"owner_cannot_leave","message":"The organization's owner cannot leave it."}
  ```

  The last admin is refused the same way (`core.rs:1306-1308`).

### 2.17 `POST {AUTH}/sign-out` — revoke

- Code: `core.rs:1467-1495`. Bearer = the **session token just deleted locally**
  (carried on a `RevocationTicket`, `core.rs:218-237`). Body: `{}`. Timeout 10 s.
- Not classified through the retry machinery; **fired exactly once**, never retried
  (`core.rs:1469-1474`; `tests.rs:2052-2073`).
- Result: `true` on 2xx **or** `401` (server already forgot the session), `false` otherwise
  (`core.rs:1486-1494`). Observed in tests as one `POST` with `Authorization: Bearer session-tok`
  (`tests.rs:2042-2049`).

### 2.18 `/organization/set-active` — **never called by the desktop**

Only the web calls it (`store.rs:131-136`, `:150-155`; `core.rs:362-367`). The desktop's
choice is local-only (§6). A CLI should not call it either unless it wants to change the
*web's* default.

### 2.19 Avatar fetch (not an Atlas endpoint)

`GET <user.image URL>` at Google/GitHub with no auth header, 5 s total timeout
(`avatar.rs:37`), 2 MiB cap checked both on `Content-Length` and while streaming
(`avatar.rs:33`, `:157-177`), content-type allow-list `image/png|jpeg|jpg|webp|gif`
(`avatar.rs:44-58`). Cached at `<config dir>/avatar-<first 8 bytes of sha256(url), hex>.<ext>`
(`avatar.rs:65-69`). Fetched once per URL; unchanged URL + file present = no request
(`avatar.rs:124-128`). Failure never blocks sign-in (`avatar.rs:23-24`).

### 2.20 Endpoints that do not exist

- No refresh endpoint, no refresh token, no revocation of the JWT (it verifies statelessly
  against JWKS until `exp`, `commands/auth.rs:264-269`, `core.rs:1449-1451`).
- No `/me`; `/get-session` is the whoami.
- No projects endpoint on the auth host; "projects" are an ingest-host concept (§6, §7).

---

## 3. The device grant, step by step

Code: `AuthCore::start_grant` (`core.rs:615-653`), `run_grant` (`core.rs:712-764`),
`poll_once` (`core.rs:667-704`), `cancel_grant` (`core.rs:656-663`), command adapter
`auth_sign_in` (`commands/auth.rs:156-211`).

1. **Resume check.** If a grant is already pending, not expired and not cancelled, return
   it instead of minting a new code (`core.rs:616-620`; `tests.rs:652-675`). A second
   click lands the user on the *same* code.
2. **`POST /device/code`** with `{"client_id":"atlas-desktop"}` (§2.1).
3. **Compute local bounds** (`core.rs:639-647`):
   - `expires_at = now + max(expires_in, 1) s`
   - `interval = max(interval s, 200 ms)` — `MIN_POLL_INTERVAL` floor so a hostile server
     cannot make the client hammer it (`core.rs:19-21`).
4. **Store the pending grant** in the one in-memory slot (`core.rs:649-651`; `core.rs:244-247`).
5. **Open the browser at `verification_uri` — the plain one.** `verification_uri_complete`
   is parsed and deliberately never opened (`core.rs:38-49`, `:61-75`; `commands/auth.rs:164-170`;
   pinned by `tests.rs:677-701`). Reason: a link carrying the code is the remote-phishing
   attack. Show `user_code` to the human for manual entry.
6. **Snapshot becomes `connecting`** with `userCode`, the plain `verificationUri`, and
   `expiresAt` as RFC 3339 (`core.rs:326-335`). The `device_code` never leaves the process
   (`tests.rs:703-730`).
7. **Poll loop** (`core.rs:713-763`), per iteration:
   1. if cancelled → `Cancelled` (no server call);
   2. if `now >= expires_at` → clear slot, `Expired`;
   3. `sleep(interval)`;
   4. if cancelled → `Cancelled`;
   5. `POST /device/token` (§2.2) and act:
      - `Pending` or `Transient` → loop at the **same** interval. A `500`, `502` or a
        dropped connection does **not** end the grant (`core.rs:706-711`; `tests.rs:517-540`).
        There is no backoff inside the grant loop.
      - `SlowDown` → `interval += 5 s` (cumulative, never reset) and loop
        (`core.rs:750-753`; `tests.rs:542-567`).
      - `Denied` → clear slot, `Err(Denied)`; message `"Connection denied in the browser."`
        (`core.rs:754-757`, `:110`).
      - `Expired` → clear slot, `Err(Expired)`; message `"That code expired. Get a new one to try again."` (`core.rs:758-761`, `:111`).
      - `Token(session_token)` →
        1. remember the previous identity (for avatar-cache pruning);
        2. **write the session file** with `identity: null, pinnedOrg: null`
           (`core.rs:306-323`, `:735-736`);
        3. clear the pending slot;
        4. **inline** `refresh_identity(previous, None)` (`core.rs:737-746`): `GET /get-session`,
           avatar fetch, `GET /organization/list`, plus one `GET /token` to read roles
           (`core.rs:469-479`). Failures here are silent and never touch the credential.
        5. return `Ok`.
8. **Expiry bound**: the local `expires_in` deadline ends a grant the server never
   finishes (`tests.rs:601-615`); the server's own deadline is 10 minutes (`core.rs:711`,
   `commands/auth.rs:175`).
9. **Cancel** (`core.rs:656-663`): sets the flag and empties the slot; idempotent; the
   loop exits at its next check and nothing is sent to the server
   (`tests.rs:617-650`). Snapshot returns to `signed-out`.

What the app does around it (`commands/auth.rs:177-208`): on success it broadcasts the
signed-in snapshot and raises the window; on `Cancelled` it just broadcasts; on any other
error it broadcasts and emits `atlas:auth-error {"message": …}`.

---

## 4. The on-disk session file

### 4.1 Path

- Directory: **Tauri's `app_config_dir()`** (`src-tauri/src/lib.rs:389-394`, fallback
  `"."` if unresolvable). This is keyed by the bundle identifier:
  - release: identifier `dev.atlas.ide` (`src-tauri/tauri.conf.json:5`) →
    macOS `~/Library/Application Support/dev.atlas.ide/` (`docs/architecture.md:255`).
  - dev profile: identifier `dev.atlas.ide.dev` (`src-tauri/tauri.dev.conf.json:4`;
    `crates/atlas-profile/src/lib.rs:56`) → `~/Library/Application Support/dev.atlas.ide.dev/`.
  - Linux / Windows: Tauri's convention is `$XDG_CONFIG_HOME/<identifier>` (default
    `~/.config/dev.atlas.ide`) and `%APPDATA%\dev.atlas.ide` respectively — verified in
    the vendored dependencies: tauri 2.11.1 `src/path/desktop.rs:238-242`
    (`app_config_dir() = dirs::config_dir().join(identifier)`), dirs 6.0.0
    `src/lin.rs:9` (`$XDG_CONFIG_HOME` only if absolute, else `~/.config`),
    `src/win.rs:8` (`FOLDERID_RoamingAppData`), `src/mac.rs:10`. Atlas ships macOS
    only today (`crates/atlas-native-agent/src/engine/config.rs:201-202`).
- File name: `atlas-session.json` (`store.rs:43-44`, `:222-224`).
- **It is not `~/.config/atlas/`** — that directory holds only `config.toml` and themes
  (`src-tauri/src/state/atlas_config.rs:2178-2191`, `:2203-2225`), and **it is not
  `.atlas/auth/…`**. The `.atlas` / `.atlas-dev` dot-directory is the per-project and home
  dot-dir for other stores (`crates/atlas-profile/src/lib.rs:103-112`); the session is not
  there. A Go CLI that wants to **share** the app's session must read/write
  `~/Library/Application Support/dev.atlas.ide/atlas-session.json` on macOS.
- Avatar cache files live beside it: `avatar-<16 hex>.<png|jpg|webp|gif>` (`avatar.rs:65-69`).

### 4.2 Schema (serde, `rename_all = "camelCase"` everywhere)

Top level `StoredSession` (`store.rs:197-220`):

```json
{
  "sessionToken": "<Better Auth session token>",
  "savedAt": "2026-10-10T12:34:56.789012+00:00",
  "identity": {
    "id": "user_1",
    "name": "Ada Lovelace",
    "email": "ada@atlas.example",
    "avatarUrl": "https://…/photo.png",
    "avatarPath": "/Users/me/Library/Application Support/dev.atlas.ide/avatar-0123456789abcdef.png",
    "orgs": [
      { "id": "org_1", "name": "Atlas", "role": "admin" },
      { "id": "org_2", "name": "Side Project", "role": null }
    ],
    "activeOrgId": "org_1"
  },
  "pinnedOrg": { "orgId": "org_1" }
}
```

Field rules:

| Field | Type | Required? | Notes | Citation |
|---|---|---|---|---|
| `sessionToken` | string | **yes** | the secret; whole account | `store.rs:200-203` |
| `savedAt` | string (RFC 3339, `chrono::Utc::now().to_rfc3339()`) | **yes** | diagnostics only; expiry is the server's business | `store.rs:204-205`, `core.rs:318` |
| `identity` | object or absent/`null` | no (`#[serde(default)]`) | absent until first successful `/get-session` | `store.rs:206-212` |
| `identity.id/name/email` | string | yes within identity | | `store.rs:103-105` |
| `identity.avatarUrl` | string or `null` | optional (absent or null; `Option<String>` without `#[serde(default)]` still loads when missing) | cache key only; never shown to UI | `store.rs:106-111` |
| `identity.avatarPath` | string or `null` | optional (absent or null) | absolute local path | `store.rs:112-114` |
| `identity.orgs` | array or absent/`null` | no (`#[serde(default)]`) | `null`/absent = **unknown**; `[]` = **known none** — must not be collapsed | `store.rs:115-131` |
| `identity.orgs[].role` | `"admin"`/`"product_owner"`/`"developer"`/`"member"`/`null` | no (`#[serde(default)]`) | unknown server value is stored as `null` | `store.rs:82-92`, `:52-74` |
| `identity.activeOrgId` | string or absent/`null` | no (`#[serde(default)]`) | seeded from web's `session.activeOrganizationId`, or set by desktop switch | `store.rs:132-142` |
| `pinnedOrg` | object or absent/`null` | no (`#[serde(default)]`) | desktop's explicit choice; `{"orgId": null}` means "chose none" | `store.rs:181-195`, `:213-219` |

Minimal valid files the loader accepts (`tests.rs:1401-1405`, `:1701-1707`):

```json
{"sessionToken":"session-tok","savedAt":"2026-07-22T00:00:00Z"}
```

```json
{"sessionToken":"session-tok","savedAt":"2026-07-22T00:00:00Z",
 "identity":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example","avatarUrl":null,"avatarPath":null}}
```

A freshly granted session is written as exactly
`{ sessionToken, savedAt, identity: null, pinnedOrg: null }` (`core.rs:312-323`) —
serde emits `null` for `None` on write.

### 4.3 I/O rules

- **Load** (`store.rs:226-232`): any read or parse error ⇒ treated as *signed out*, not an
  error.
- **Save** (`store.rs:234-265`): `create_dir_all(dir)`; serialise pretty; open
  `atlas-session.tmp` (`path.with_extension("tmp")`) with `write|create|truncate` and
  **mode `0o600` from the first byte** (unix); write; `rename` over `atlas-session.json`
  (atomic replace); then `chmod 0600` again on the final path as belt-and-braces
  (`store.rs:277-285`). Test: `tests.rs:448-471` asserts `mode & 0o777 == 0o600`.
- **Clear** (`store.rs:267-275`): `remove_file`; `NotFound` is success (idempotent).
- All load→modify→save sequences are serialised under one mutex held only across
  synchronous sections (`core.rs:248-254`, `:273-280`). A refresh that re-reads after its
  network calls and finds the file gone must not write it back (`core.rs:551-566`;
  `tests.rs:2124-2163`).
- **Why not the keychain** (`store.rs:7-33`): unsigned binary + auto-updater ⇒ a keychain
  ACL re-prompt after every update. Ratified exception.

### 4.4 What is a secret

- `sessionToken` — yes (the whole account; `mod.rs:20-24`). Never logged, never crosses
  IPC.
- Access JWT — secret, **never written to disk** (`store.rs:35-36`); held in memory only.
- `device_code` — secret during the grant; in memory only, never logged (`core.rs:32-34`).
- `user_code`, `verification_uri` — display strings, not secrets (`mod.rs:109-115`).
- `avatarUrl` — not a credential but privacy-sensitive (a beacon to Google/GitHub); the
  app never hands it to the renderer (`store.rs:106-111`).

---

## 5. `AuthSnapshot` and the state machine

Rust: `src-tauri/src/auth/mod.rs:94-154`. TS mirror: `src/features/auth/lib/auth-api.ts:17-114`.
Serde: internally tagged on `"status"`, variant names kebab-case, fields camelCase.

```json
{"status":"signed-out"}
```

```json
{"status":"connecting",
 "userCode":"ABCD-1234",
 "verificationUri":"https://app.example/device",
 "expiresAt":"2026-10-10T12:44:56+00:00"}
```

```json
{"status":"signed-in",
 "user":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example",
         "avatarPath":"/…/avatar-….png"},
 "orgs":[{"id":"org_1","name":"Atlas","role":"admin"},
         {"id":"org_2","name":"Side Project","role":null}],
 "activeOrgId":"org_1",
 "commsOrgId":"org_1"}
```

Field semantics:

- `user` — `null` only between holding a credential and the first successful profile fetch
  (`mod.rs:121-127`). `avatarPath` is a local absolute path or `null`; the remote URL is
  never exposed (`mod.rs:42-46`).
- `orgs` — `null` = not known yet; `[]` = known to be none; otherwise the list
  (`mod.rs:128-138`).
- `activeOrgId` — resolved billing/display org (§6); `null` when `orgs` is `null`/empty
  (`mod.rs:139-142`).
- `commsOrgId` — the org the chat socket dials; honours an explicit "none"; not filtered by
  membership (`mod.rs:143-152`).
- Nothing in the snapshot is a credential (`mod.rs:94-99`; `tests.rs:1365-1393`).

State machine (`core.rs:326-360`, derived from in-memory slot + file on every call, no
stored state):

```
signed-out ──start_grant──▶ connecting ──Token──▶ signed-in
     ▲                         │ (Denied/Expired/Cancelled/local deadline)
     │                         ▼
     └──────────────────── signed-out
signed-in ──sign_out (local unlink first)──▶ signed-out
signed-in ──validate_once gets 401 on /token──▶ signed-out (+ "atlas:auth-signed-out")
```

- `connecting` wins while a non-expired grant is pending, even if a file exists
  (`core.rs:327-335`).
- There is deliberately **no "verifying" state** (`mod.rs:96-99`).
- `Validation` (one revalidation outcome, `core.rs:200-216`):
  `NoCredential | Confirmed | Rejected (401, cleared) | Denied (403, nothing changed, no retry) | Indeterminate{retry_after}`.

### Role enum

`store.rs:52-59`, `#[serde(rename_all = "snake_case")]`, highest privilege first:
`"admin"`, `"product_owner"`, `"developer"`, `"member"`. Unknown strings decode to `null`
and the org is still listed (`store.rs:61-74`; `tests.rs:1601-1618`). **Owner is not a
role** — it is the creator, learned from `roster.ownerId` and surfaced as
`OrgMember.isOwner` (`core.rs:1597-1601`; `CONTEXT.md:129`). The server's contract is the
same four values (`docs/research/agent-org-teams-api-access.md:62`).

Other public shapes (camelCase on the wire to the UI):

- `OrgMember { id (membership), userId, name, email, role, createdAt, avatarPath, isOwner }` (`core.rs:1583-1602`).
- `OrgInvitation { id, email, role, status, expiresAt, acceptUrl }` (`core.rs:1609-1618`).
- `CreatedOrg { id, name }` (`core.rs:1566-1570`).

### Events the app emits (for parity, not required by a CLI)

`atlas:auth-changed` (full snapshot, every transition), `atlas:auth-error {message}`
(grant ended without a token), `atlas:auth-signed-out {message}` (server rejected the
stored session; message `"Your Atlas session ended. Sign in again to reconnect."`)
(`commands/auth.rs:7-18`, `:34`, `:534-539`).

---

## 6. Active org: how it is chosen and persisted; org vs project

### 6.1 Resolution (`store.rs:145-166`, `core.rs:336-356`)

```
active_org_id  = identity.orgs is None            → None
               = identity.activeOrgId ∈ orgs       → that id
               = otherwise                          → orgs[0].id   (None if orgs == [])
comms_org_id   = pinnedOrg is Some(pin)            → pin.orgId   (may be None = "no org")
               = pinnedOrg is None                 → active_org_id
```

Tests: web-set id wins over list order (`tests.rs:1742-1757`); an id no longer in the list
falls back to first (`tests.rs:1758-1768`); switch persists across restart
(`tests.rs:1800-1805`); clearing the pin returns billing to the fallback but turns the
socket off (`tests.rs:1807-1814`).

### 6.2 Sources of `identity.activeOrgId`

1. **Seed from the web**: `session.activeOrganizationId` from `/get-session` is used only
   when the session has never chosen (`core.rs:568-588`). It is normally `null` for a
   device-granted session (`core.rs:1546-1552`).
2. **Desktop switch** `AuthCore::set_active_org(Option<String>)` (`core.rs:362-401`,
   command `auth_set_active_org` `commands/auth.rs:223-245`): writes
   `pinnedOrg = {orgId}` **and** `identity.activeOrgId = orgId` (if an identity exists),
   locally only — the server is **not** told. Works before the first profile fetch
   (`tests.rs:1921-1947`). An id not in the list is stored anyway (`core.rs:379-382`).
3. **Refresh preserves the pin** (`core.rs:581-588`; `tests.rs:1833-1876`, `:1878-1919`).
4. **A new grant clears both** (`core.rs:306-323`; `tests.rs:1949-1972`).

### 6.3 Why two ids

`activeOrgId` is what every gateway request bills via `atlas-org` and what telemetry
reports (`core.rs:369-376`; `commands/auth.rs:223-230`). `commsOrgId` is what the chat
socket dials and may be `null` for a local-only org (`commands/comms.rs:196-215`,
`mod.rs:143-152`).

### 6.4 "Org" vs "project" in auth terms

- **Organisation** = the tenant a signed-in user belongs to; a roster of members with one
  role each (`CONTEXT.md:127-128`). Auth knows orgs only by **server id** (what
  `/organization/list` returns). The app also has *local* org rows with a random local id
  and an optional `remoteId` = server id (`commands/auth.rs:232-234`;
  `commands/telemetry.rs:51-56`; `crates/atlas-comms/src/lib.rs:61-66`). Only the server id
  ever goes on the wire.
- **Project** = a folder opened in Atlas, tagged to a (local) organisation
  (`CONTEXT.md:28`). Auth has no project concept and sends no project id anywhere.
- **Workspace** = the cloud aggregate a Project binds to on the ingest host
  (`CONTEXT.md:122`); ids appear as `workspaceId`/`?workspace=` on ingest/artifacts calls
  (§7), never on the auth host.

---

## 7. How other subsystems obtain the token and what they send

Every consumer goes through `AuthState.core().mint_access_token()` — i.e. a fresh
`GET {AUTH}/token` with the stored session token — and sends the resulting **JWT** as
`Authorization: Bearer <jwt>`. The session token itself never leaves the auth host.
Only Rust holds the bearer; the renderer sees none (`crates/atlas-comms/src/rest.rs:3-7`,
`commands/comms.rs:3-5`, `commands/artifacts_cloud.rs:4-6`).

| Subsystem | Token source | Cache / refresh | Host | Org / project carried as | Citation |
|---|---|---|---|---|---|
| **Native agent → AI gateway** | `AccountTokenSource` | `AtlasExternalAuth`: cache per connection, re-mint at `exp − 60 s` (fallback TTL 600 s), always re-mint on the engine's 401 refresh | `https://ai.tryatlas.cc/v1` | header **`atlas-org: <activeOrgId>`**, omitted when `None`/empty | `src-tauri/src/commands/agents.rs:2347-2399`, `:1219-1239`; `crates/atlas-native-agent/src/engine/auth.rs:38-49`, `:143-217`; `vendor/atlas-engine/atlas-engine-api/src/atlas_chat/org.rs:42-51`; `vendor/atlas-engine/atlas-engine-api/src/endpoint/chat_completions.rs:77-85` |
| **Entitlement probe** | mint per call | none | `GET {AI}/models` | `atlas-org` header when active org set | `src-tauri/src/commands/agent_entitlement.rs:157-189` |
| **Model catalogue fetch** | mint | on-disk catalogue cache | `GET {AI}/catalogue` | `atlas-org` header | `crates/atlas-native-agent/src/engine/catalog_cache.rs:32`, `:157`, `:278` |
| **Memory extraction via gateway** | mint per call | none | `POST {AI}/chat/completions` | `atlas-org` header | `src-tauri/src/commands/memory_extract.rs:389-428` |
| **Timeline / capture sync (`atlas-checkpoint`)** | closure `token_provider` → mint (blocking) | mint per pass; refresh once on 401 per drain (`crates/atlas-checkpoint/src/sync.rs:25-30`) | `https://ingest.tryatlas.cc` | `orgId` as **query** (`/workspaces/slug-available?orgId=&slug=`, `/workspaces?orgId=`) or **body** (`POST /workspaces`, `POST /workspaces/connect` with `orgId`, `workspaceId`, `slug`, `visibility`, `rootCommitSha`, `gitUrl`); `POST /ingest` and `PUT /blobs/{key}` carry no org (the artifact body carries the workspace) | `src-tauri/src/lib.rs:434-438`; `src-tauri/src/commands/capture.rs:2764-2805`; `crates/atlas-checkpoint/src/sync.rs:75-85`, `:465-477`, `:555-559`, `:606-610`, `:703-716`, `:854-858`, `:935-948` |
| **Artifacts cloud reads (`atlas-artifacts`)** | `AppTokenSource` | reuse 240 s, drop on 401 | ingest host | `?org=<server org id>` on every GET; `?workspace=<project id>` optional | `src-tauri/src/commands/artifacts_cloud.rs:62-93`; `crates/atlas-artifacts/src/client.rs:111-128`, `:175-182` |
| **Artifacts realtime socket** | same | — | `wss://ingest…/ws?org=<org>&workspace=<project>` | JWT in `Sec-WebSocket-Protocol: atlas.v1, atlas.ticket.<jwt>` | `crates/atlas-artifacts/src/lib.rs:108-119`; `crates/atlas-artifacts/src/socket.rs:7-16` |
| **Team chat REST (`atlas-comms`)** | `AppTokenSource` | reuse 240 s, drop on 401 | `https://chat.tryatlas.cc` | `?org=<server org id>` appended to **every** path; 20 s timeout | `src-tauri/src/commands/comms.rs:26-61`; `crates/atlas-comms/src/rest.rs:115-118`, `:163-186` |
| **Team chat socket** | same | — | `wss://chat…` | `Sec-WebSocket-Protocol: atlas.v1, atlas.ticket.<jwt>`; targets `commsOrgId` | `crates/atlas-comms/src/conn.rs:3-13`; `commands/comms.rs:196-215` |
| **Org tool server (`atlas_org`)** | wraps `AuthCore` + the two clients above | — | — | per-session `OrgScope` (org + workspace) | `src-tauri/src/commands/org_server/adapter.rs:28`, `:54-55`, `:172` |
| **Telemetry (PostHog)** | **no token sent** | — | PostHog | identity from the snapshot only | see below |
| **Feedback** | no token; identity from snapshot | — | via telemetry | `account_id`, `account_email`, `active_org_id` properties | `src-tauri/src/commands/feedback.rs:112-142` |
| **Shared memory / memory sync** | **does not use the account token at all** | — | loopback only | — | no `mint_access_token`/`bearer_auth`/`tryatlas` in `commands/shared_memory.rs`, `commands/memory_server/host.rs`, `commands/memory_indexer.rs`; the memory MCP server's own per-session loopback bearer is unrelated (`commands/memory_server/tokens.rs:228-239`) |

Telemetry identity (`commands/auth.rs:57-136`; `src-tauri/src/telemetry/mod.rs:117-131`,
`:534-585`): on every `signed-in` snapshot with a `user`, **and only if the setting
`linkTelemetryToAccount` is on**, PostHog `$identify` is sent with
`$set: {email, name, atlas_account: true, atlas_org_count, atlas_active_org_id}` and
`$set_once: {atlas_device_id}`; otherwise identity is reset to the device. The org group
attached to every event is the **local** org id (`$groups: {organisation: <local id>}`),
with name/role only for a synced org (`telemetry/mod.rs:133-158`, `:587-620`;
`commands/telemetry.rs:37-80`). Telemetry's own UA is `Atlas/<version> (telemetry)`
(`telemetry/mod.rs:814`). Nothing from the auth module is sent to telemetry beyond this.

Sign-out also drops the native agent's cached JWT by dropping the connection, because the
JWT outlives the revoked session for up to ~9 minutes (`commands/auth.rs:261-272`).

---

## 8. What a Go reimplementation must get right

**Identity of the client**

- `client_id` is the literal string **`atlas-desktop`** (`core.rs:15-16`), on both
  `/device/code` and `/device/token`. The server's device plugin matches on it. (Whether
  the server would accept a different id for a CLI is a server-side question; using this
  one is what "logs in exactly the way the app does".)
- `grant_type` is `urn:ietf:params:oauth:grant-type:device_code` (`core.rs:17`).
- **No scopes, no client secret, no PKCE, no redirect URI** anywhere (`mod.rs:3-6`;
  request bodies §2.1–2.2).
- **No `User-Agent` is required**: the auth client sends none (`commands/auth.rs:47`).
  The gateway UA the engine sends (`<originator>/<ver> (<os> <ver>; <arch>) …`,
  `vendor/atlas-engine/login/src/auth/default_client.rs:160-184`) is informational; no code
  in this repo depends on the server checking it.
- Identity headers `X-Atlas-User` / `X-Atlas-Org` / `X-Atlas-Role` are **ignored** by the
  gateway (`docs/reference/atlas-ai-api.md:112-116`) — do not send them.

**Token types**

- The `/device/token` `access_token` (`token_type: "Bearer"`) is the **session token**:
  opaque, long-lived (7-day rolling, `store.rs:201-202`), used **only** against
  `{AUTH}/*` as `Authorization: Bearer <session>`.
- The `/token` `token` is the **access JWT**: audience `atlas`, EdDSA, ~10 min TTL; used
  against ai / ingest / chat as `Authorization: Bearer <jwt>` (§2.3, §7). Do not persist
  it (`store.rs:35-36`). Re-mint at **T−60 s** (`engine/auth.rs:38-42`;
  `atlas-ai-api.md:1076-1077`); on a gateway `401 token_expired` re-mint **once** and retry
  (`atlas-ai-api.md:722-723`; `engine/auth.rs:208-216`). Reading claims: base64url
  without padding, second segment; signature verification is not needed client-side
  (`core.rs:1620-1627`).
- Mint sparingly: the auth host is rate-limited at 100 req/60 s per IP and the app's
  clients cache a minted JWT for 240 s (`TOKEN_REUSE`, `crates/atlas-comms/src/rest.rs:118`, `crates/atlas-artifacts/src/client.rs:114`) or until
  `exp − 60 s`.

**Org on the wire**

- Gateway: header **`atlas-org: <server org id>`** (case-insensitive; the doc spells it
  `Atlas-Org`). Omit it for personal attribution; **never send it empty**
  (`org.rs:48-50`). The org must be in the JWT's `orgs` claim or you get
  `403 org_not_covered` (`atlas-ai-api.md:97-101`, `:726`). An account entitled only
  through its org gets `403 no_entitlement` without the header (`org.rs:4-10`).
- Chat and artifacts REST: `?org=<server org id>` query on every call
  (`rest.rs:170-172`; `client.rs:179`).
- Ingest workspace routes: `orgId` in the query or JSON body (§7 row).
- WebSockets: JWT rides as `Sec-WebSocket-Protocol: atlas.v1, atlas.ticket.<jwt>`; the
  server echoes only `atlas.v1` (`conn.rs:9-13`).

**Failure handling the server expects (and the app's own rules)**

- Only a **`401` on `GET /token`** may delete the stored session (`core.rs:1388-1392`;
  `mod.rs:17-18`). A `401` from `/organization/list` or any other call must not
  (`core.rs:441-445`).
- `/get-session` returning `200 null` is **not** a sign-out signal (`core.rs:407-409`).
- `429`/`5xx`/transport ⇒ keep everything and retry with the backoff: exponential from
  **1 s** to a **300 s ceiling**, delay drawn from `[cap/2, cap]`, `Retry-After` honoured
  but **clamped to `[1 s, 300 s]`**, attempt counter always advances, **no attempt limit**
  (`backoff.rs:21-25`, `:59-87`; `tests.rs:974-1021`). The cost at the ceiling (12 req/h)
  is what keeps a signed-in idle client under the rate limit (`backoff.rs:13-17`).
- `403`/`400`/`404` ⇒ `Denied`: keep the credential, do not retry (`core.rs:135-142`;
  `tests.rs:890-907`).
- Gateway error envelope is OpenAI-shaped `{"error":{"message","type","code","param"}}`;
  branch on `code` (`atlas-ai-api.md:118-160`). `402 cap_exceeded` is never retried;
  `429 rate_limited` is (`Retry-After` set); `503 atlas_backstop_tripped` stop
  (`atlas-ai-api.md:714-752`).

**Device grant details**

- Poll floor 200 ms regardless of the server's `interval` (`core.rs:21`, `:645`).
- `slow_down` ⇒ `interval += 5 s`, cumulative (`core.rs:750-753`).
- Keep polling through 5xx / dropped connections; stop only on `access_denied`,
  `expired_token`, local `expires_in` deadline, or user cancel (`core.rs:706-764`).
- Open the **plain** `verification_uri` and show `user_code`; never open or print
  `verification_uri_complete` to a browser (`core.rs:61-75`).
- Second "sign in" while a grant is pending ⇒ reuse it, don't mint another
  (`core.rs:609-620`).

**Sign-out order**

- Delete the local file (and cached avatars) **first**, then `POST /sign-out` with the
  old session token as Bearer, body `{}`, once; `2xx` or `401` = revoked; anything else =
  tell the user the server session may outlive the local one (`core.rs:1430-1495`;
  `tests.rs:2006-2050`). Also discard any cached JWT.

**Session file compatibility (if the CLI shares the app's file)**

- Path §4.1; name `atlas-session.json`; camelCase keys exactly as §4.2; mode `0600`;
  write to `atlas-session.tmp` in the same directory with `0600` and `rename` over
  (`store.rs:241-265`). Tolerate a file with only `sessionToken` + `savedAt`
  (`tests.rs:1395-1427`). Never collapse `orgs: null` into `[]` (`store.rs:115-131`).
  Preserve `pinnedOrg` on any rewrite (`core.rs:568-588`). A corrupt file means
  "signed out", not an error (`store.rs:226-232`).

**After sign-in, to render a full identity like the app** (`core.rs:532-605`): `GET
/get-session` → `GET /organization/list` → `GET /token` (for roles) → join names with the
JWT `orgs` claim; a role missing from a *readable* claim is `null`, an *unreadable* token
keeps the previously known roles (`core.rs:469-499`; `tests.rs:1620-1692`).

---

## Appendix A — Tauri command surface (what the UI can ask Rust)

`src-tauri/src/commands/auth.rs`: `auth_snapshot` (:146), `auth_sign_in` (:156),
`auth_cancel_sign_in` (:215), `auth_set_active_org` (:236), `auth_sign_out` (:261, returns
`bool` = server confirmed), `auth_create_org` (:306), `auth_check_org_slug` (:329),
`auth_refresh` (:347), `auth_delete_org` (:365), `auth_list_members` (:388),
`auth_list_invitations` (:402), `auth_invite_member` (:418), `auth_cancel_invitation`
(:437), `auth_update_member_role` (:451), `auth_remove_member` (:469), `auth_leave_org`
(:487); launch path `restore_on_launch` (:522) — broadcast the stored snapshot
immediately, then `revalidate` forever on the backoff until `Confirmed`/`Rejected`/`Denied`.

## Appendix B — ADR / glossary coverage

No ADR covers the device grant or session storage; the only auth-adjacent ADR is 0014
(organisation tool server acts as the user). `CONTEXT.md:118-135` defines Organisation,
Member, Owner, Workspace, Project. The credential-storage deviation is recorded in the
archived port spec D14 (`docs/archive/atlas-agent-codex-port-spec.md:110`) and in
`store.rs:28-33`.
