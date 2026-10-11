# ADR-0005: Proxy usage is fed through the same cumulative-delta recorder as the desktop (O5)

**Status:** Accepted (2026-10-10, locked by the user with SPEC D1–D19). Records SPEC §1.2 D17 (formerly §10 O5).

## Context

The desktop records usage from **cumulative** reports. `record_usage_delta`
diffs each report against `usage_cursor`, adds the difference into
`usage_delta(session_id, turn_seq)`, grows `token_totals`, and replaces the
cursor (evidence-capture.md:437). The proxy sees **per-request** usage: one
Anthropic `usage` block per request, and a Claude Code turn makes many
requests in its tool loop.

SPEC §7.2 suggests writing per-request usage straight into `usage_delta` and
keeping the cursor equal to the running sum. That gives the right rows, but
it is a second write path for the same table, with its own rules for model
attribution and `sent → pending`.

The desktop's importer for Claude Code transcripts sums per-request usage
(deduped by `requestId`) into the session (evidence-capture.md:697). So
"sum of per-request usage" is already what the desktop shows for Claude Code
sessions it imported.

## Decision

- **One write path.** The proxy keeps a per-session running sum of
  per-request usage and calls the same `RecordUsage(sessionID, turnSeq,
  model, cumulative)` the ACP recorder calls. The diff against the cursor then
  equals the request's own usage, and the cursor equals the running sum, as
  SPEC §7.2 asks, through code that matches the desktop's rules.
- **The running sum is seeded from `usage_cursor`** when a session is first
  seen in a process, so a restarted `atlas shell` does not trip the
  "counter restarted" branch.
- **One request counts once.** For streamed Anthropic responses, usage is
  read from `message_start` (input and cache fields) and the **last**
  `message_delta` (output), not summed across events. Requests are keyed by
  the response `id` (or the `request-id` header) so a retried or replayed
  response is not counted twice.
- Field mapping (Anthropic): `input_tokens → input`, `output_tokens → output`,
  `cache_creation_input_tokens → cache_creation`,
  `cache_read_input_tokens → cache_read`, `reasoning = 0`. Context gauges are
  not written from the proxy.

## Consequences

- `usage_delta` rows and `token_totals` for proxy sessions match what the
  desktop importer would produce for the same Claude Code session.
- **Not verifiable here:** whether the desktop's Usage tab reads a
  CLI-written store correctly. That is a manual check during Phase 6
  acceptance: run one session, open it in the desktop, compare totals with the
  Anthropic console.
- OpenAI Responses and Gemini mappings get the same treatment in Phase 7,
  each with a fixture.
