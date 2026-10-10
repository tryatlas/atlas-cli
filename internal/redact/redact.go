package redact

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Placeholder replaces every redacted span. It is itself held out by every
// layer, which is what makes redaction idempotent.
const Placeholder = "[REDACTED]"

// Category names one of the six layers, spelled as stored in
// agent_session.redaction_counts (SPEC §6.3).
type Category string

// The six layers (SPEC §6.5).
const (
	Entropy          Category = "entropy"
	VendorRule       Category = "vendor_rule"
	ProviderToken    Category = "provider_token"
	CredentialedURI  Category = "credentialed_uri" //nolint:gosec // G101: a category name, not a credential
	ConnectionString Category = "connection_string"
	CredentialValue  Category = "credential_value" //nolint:gosec // G101: a category name, not a credential
)

// priority decides which category a span is counted under when several
// layers flag overlapping text: the most specific explanation wins.
var priority = map[Category]int{
	CredentialedURI:  0,
	ConnectionString: 1,
	VendorRule:       2,
	ProviderToken:    3,
	CredentialValue:  4,
	Entropy:          5,
}

// Counts tallies redacted spans per category.
type Counts map[Category]int

// Total is the number of spans redacted.
func (c Counts) Total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

// Add merges other into c.
func (c Counts) Add(other Counts) {
	for k, v := range other {
		c[k] += v
	}
}

// MarshalJSON writes {"<category>": n} with keys sorted and zero counts left out.
func (c Counts) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(c))
	for k, v := range c {
		if v != 0 {
			keys = append(keys, string(k))
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		b.Write(kj)
		fmt.Fprintf(&b, ":%d", c[Category(k)])
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// Result is redacted text and what was replaced.
type Result struct {
	Text   string
	Counts Counts
}

// Error reports that the redactor failed. The caller must store nothing for
// that content (SPEC §6.4 "Redaction failure").
type Error struct{ cause any }

func (e *Error) Error() string { return fmt.Sprintf("redaction failed: %v", e.cause) }

// Flat runs the flat pass over text (the desktop's `redact`). Capture derives
// session titles from it.
func Flat(text string) (res Result, err error) {
	defer recoverInto(&err)
	return flat(text), nil
}

// Auto walks JSON (an object or array, or JSONL of two or more such
// lines) structurally and re-serialises it compactly; anything else takes the
// flat pass (the desktop's `redact_auto`). Every stored body, tool argument,
// tool result and edit patch goes through it.
func Auto(text string) (res Result, err error) {
	defer recoverInto(&err)
	if r, ok := redactJSON(text); ok {
		return r, nil
	}
	return flat(text), nil
}

func recoverInto(err *error) {
	if p := recover(); p != nil {
		*err = &Error{cause: p}
	}
}

// span is a byte range [start, end) flagged by one layer.
type span struct {
	start, end int
	cat        Category
}

// flat runs every layer over text and replaces the merged spans.
func flat(text string) Result {
	counts := Counts{}
	if text == "" {
		return Result{Text: text, Counts: counts}
	}
	lower := asciiLower(text)
	var spans []span
	spans = append(spans, credentialedURISpans(text)...)
	spans = append(spans, connectionStringSpans(text, lower)...)
	spans = append(spans, vendorSpans(text, lower)...)
	spans = append(spans, providerSpans(text)...)
	spans = append(spans, credentialValueSpans(text, lower)...)
	spans = append(spans, entropySpans(text)...)
	return apply(text, spans, counts)
}

// apply merges overlapping spans, counts each merged span once under its
// highest-priority category, and replaces it with the placeholder.
func apply(text string, spans []span, counts Counts) Result {
	if len(spans) == 0 {
		return Result{Text: text, Counts: counts}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var merged []span
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start < merged[n-1].end {
			last := &merged[n-1]
			if s.end > last.end {
				last.end = s.end
			}
			if priority[s.cat] < priority[last.cat] {
				last.cat = s.cat
			}
			continue
		}
		merged = append(merged, s)
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, s := range merged {
		b.WriteString(text[prev:s.start])
		b.WriteString(Placeholder)
		prev = s.end
		counts[s.cat]++
	}
	b.WriteString(text[prev:])
	return Result{Text: b.String(), Counts: counts}
}
