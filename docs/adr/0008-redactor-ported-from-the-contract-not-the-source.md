# ADR-0008: The redactor is ported from its contract, not its source

**Status:** Proposed (2026-10-10). Phase 2. Needs the user's review.

## Context

SPEC §6.5 and `redact-README.md` give the redactor's contract: six layers,
`[REDACTED]`, idempotence, per-category counts, the JSON-aware walk, and the
values held out (placeholders, `${VAR}`, mask runs, ordinary identifiers, git
SHAs). They do not give its parameters: the entropy threshold, the
provider-prefix list, the credential key names, the connection-string forms,
or which JSON keys count as ids, paths and URLs. The desktop's generated rule
table (`crates/atlas-redact/src/vendor_rules.rs`) and its generator are not
in the hand-off folder. SPEC §6.5 asks for "the same ruleset so counts agree".
That cannot be met exactly without them.

Nothing on disk depends on the two redactors being identical. A stored text
holds `[REDACTED]` where a secret was, and `redaction_counts` holds a tally.
The desktop never re-redacts rows the CLI wrote. What matters is that the
CLI redacts at least as well and does not destroy ordinary text.

## Decision

- **Vendored rules are data.** `tools/genrules` turns the upstream betterleaks
  config (`betterleaks/betterleaks@43f0ee93`, MIT, licence alongside) into
  `internal/redact/vendor_rules.json`, embedded in the binary. It keeps 305 of
  466 rules, close to the desktop's "~310". It keeps a rule when its regex
  compiles under Go RE2 and its filter can be evaluated on the matched value
  alone: an entropy bound, `matchesAny`/`containsAny`/`!matchesAny` on the
  secret. Path clauses are dropped; the redactor sees text, not files. The 161
  exclusions are listed in the file with reasons: 115 need betterleaks'
  tokenizer (`tokenRatio`), 37 are composite, 5 are path-scoped or path-only,
  and 4 have filters that are programs. The upstream global filter is applied
  to every vendor finding.
- **The other five layers are written here, with these parameters:**
  - *entropy*: runs of 20 or more base64/base64url characters with ≥ 3.5
    bits/char, containing a digit and a letter. Held out: hex-only strings
    (hashes, SHAs), UUIDs, paths, identifiers whose character-class runs
    average ≥ 3 (camelCase, snake_case, kebab-case), and `sha512-…` / `h1:…`
    integrity digests;
  - *provider prefixes*: 37 token formats (`sk-`, `sk-ant-`, `ghp_`, `xox…`,
    `sb_secret_`, `AKIA`, `AIza`, `glpat-`, `npm_`, …), each bounded on both
    sides;
  - *credentialed URIs*: the password in `scheme://user:password@host`,
    except a short number (that is `host:port@` confusion);
  - *connection strings*: JDBC `password=`, semicolon strings that name a
    server (ADO.NET, ODBC, Azure `AccountKey`), and libpq keyword DSNs;
  - *credential key/value*: keys split into words. A key is a credential
    when it contains a word such as password, secret, token, auth or
    credential, or a pair such as api key, access key or client secret, and
    does not end in a word that describes one (`_count`, `_name`, `_url`,
    `_length`, …). `PWD` and file names (`auth.rs:12`) are not keys. A value
    is left alone when it is a placeholder, a reference (`$X`, `${X}`,
    `settings.SECRET`), code (`get()`, `x[i]`), a number range, a short
    number, or a bare word after `key: ` (prose).
- **Overlaps count once.** Spans flagged by several layers merge. The merged
  span counts under the most specific layer: URI, connection string, vendor
  rule, provider prefix, credential value, then entropy.
- **The JSON walk** keeps key order and number literals, escapes strings as
  serde_json does (no HTML escaping), and leaves values under id and path keys
  untouched. Values under URL keys get only URI-credential and query-parameter
  redaction. Values under credential keys are redacted whole, keeping an auth
  scheme ("Bearer [REDACTED]"). Array elements inherit their parent's key.
- **Failure is an error, not a panic.** `Flat` and `Auto` return `*Error`
  when anything inside panics, so the recorder can write nothing and flag the
  session.

## Consequences

- `redaction_counts` for one text can differ between the CLI and the desktop.
  Both are tallies of what was removed, so no reader breaks; the numbers just
  are not comparable.
- Some secrets the desktop would catch (the 115 tokenizer-gated rules) are
  caught here only when the entropy or key/value layers see them.
- Tuning is evidence-led. Every false positive found by scanning this repo's
  docs is a regression test in `TestHoldOuts`.
- **To close the gap:** with `crates/atlas-redact` (`lib.rs`,
  `vendor_rules.rs`, `tests/negatives.rs`, `tests/parity_memory_delta.rs`),
  the parameters and the fixture corpus can be matched and this ADR replaced.
