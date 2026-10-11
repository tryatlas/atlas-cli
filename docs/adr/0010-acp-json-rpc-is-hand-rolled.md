# ADR-0010: The ACP client's JSON-RPC is hand-rolled; acp-go-sdk is not a dependency

**Status:** Accepted (2026-10-11). Applies SPEC §8.1 and evidence-acp.md §0.

## Context

`atlas run` speaks ACP to every registry agent over newline-delimited
JSON-RPC on the child's stdio (evidence-acp.md §0). The desktop uses the Rust
SDK `agent-client-protocol =2.0.0` with schema crate
`agent-client-protocol-schema 1.5.0` and the `unstable` features. There is no
official Go SDK under `github.com/agentclientprotocol`. The candidate is
`github.com/coder/acp-go-sdk`, checked on 2026-10-11:

| fact | value |
|---|---|
| latest version | `v0.13.5` (2026-06-02); tags since `v0.4.3`, still 0.x |
| schema | generated from the spec repo's JSON schema tagged `0.13.5` (`schema/version`), stable and unstable files |
| dependencies | none (`go.mod` has no `require`); Go 1.21; Apache-2.0 |
| size | ~15 400 lines, 9 500 of them generated types |
| coverage | every method and notification Atlas uses exists: `session/list|resume|close|delete`, `set_config_option`, `logout`, elicitation, typed `terminal` auth methods, `usage_update`, `session_info_update`, `config_option_update`, `additionalDirectories` |

The SDK's spec tag (`0.13.5`) and the Rust schema crate's version (`1.5.0`)
are numbered independently; nothing in either repo maps one to the other, so
"same schema level" can only be checked field by field. Doing that turned up
one difference. evidence-acp.md §3.1 has the desktop send
`clientCapabilities.session.configOptions.boolean`, but the SDK's
`ClientCapabilities` (stable and unstable) has no `session` key. The desktop's
behaviour is the reference (evidence wins), so the CLI sends that key, and a
typed SDK struct could not express it.

What else using the SDK would mean:

1. **Strict unions.** Each generated union (`SessionUpdate`, `ContentBlock`,
   `AuthMethod`, …) fails the whole message with "invalid variant payload" when
   one field has an unexpected shape, and "no matching variant for union" for
   an unknown `sessionUpdate`. The desktop drops unknown variants
   (`_ => {}`) and parses tolerantly. The recorder also needs the raw JSON
   (`rawInput`, `rawOutput`, `_meta.terminal_*`), so the generated types would
   be bypassed anyway.
2. **Transport limits that kill the session.** Lines over 10 MiB end the
   connection (a scanner limit), and so does a notification backlog over 1024
   entries. Neither limit exists on the desktop.
3. **Logging of content.** Parse failures and unknown messages are logged
   with the raw line through `slog`, by default to stderr. That can include
   prompts and file contents that never passed the redactor. `SetLogger` can
   only be called after the reader goroutine has started.

The part of the SDK that is hard to get right is small: request and
response correlation, `$/cancel_request` for an abandoned request, one serial
queue for notifications, and the rule that a response is delivered only after
the notifications that preceded it on the wire. That fits in a few hundred
lines with tests.

## Decision

1. Hand-roll the JSON-RPC layer in `internal/agents/acp`:
   - newline-delimited, no line-length limit;
   - one reader goroutine that puts notifications **and** responses, in
     wire order, onto one unbounded serial queue, so a `session/prompt`
     result is seen only after the `session/update`s before it;
   - agent→client requests are served off that queue on their own
     goroutines (the desktop's non-blocking dispatch);
   - a cancelled caller context sends `$/cancel_request`;
   - nothing logs message bodies or headers.
2. Message types are local, tolerant structs: unknown fields are ignored,
   unknown variants are dropped, and raw JSON is kept where the recorder
   needs it. Key spellings follow evidence-acp.md and are pinned by fixture
   tests, one per method and notification.
3. `acp-go-sdk` is not a dependency. Its `schema/schema.unstable.json`
   (v0.13.5) is the cross-check used when a key spelling is in doubt;
   evidence-acp.md wins when the two disagree.

## Consequences

- No third-party code sits on the protocol path. The CLI owns the
  tolerance rules the desktop has.
- A new ACP field means a change to a local struct. Unknown variants are
  harmless in the meantime, because they are dropped.
- If an official Go SDK appears that is tolerant, has no line limit and
  accepts `clientCapabilities.session`, this ADR is the place to revisit.
