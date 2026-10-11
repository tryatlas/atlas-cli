package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// node is an order-preserving JSON value. Numbers keep their literal.
type node struct {
	kind   byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	str    string
	b      bool
	keys   []string
	values []*node
}

// redactJSON applies the structural pass when text is a JSON object or array,
// or JSONL of at least two such lines. ok is false when text is neither.
func redactJSON(text string) (Result, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return Result{}, false
	}
	counts := Counts{}
	if n, ok := parseContainer(trimmed); ok {
		var b strings.Builder
		walk(n, "", counts)
		encode(&b, n)
		return Result{Text: b.String(), Counts: counts}, true
	}
	// JSONL: two or more non-blank lines, each an object or array.
	lines := strings.Split(text, "\n")
	parsed := make([]*node, len(lines))
	nonBlank := 0
	for i, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if l[0] != '{' && l[0] != '[' {
			return Result{}, false
		}
		n, ok := parseContainer(l)
		if !ok {
			return Result{}, false
		}
		parsed[i] = n
		nonBlank++
	}
	if nonBlank < 2 {
		return Result{}, false
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if parsed[i] == nil {
			b.WriteString(line)
			continue
		}
		walk(parsed[i], "", counts)
		encode(&b, parsed[i])
		if strings.HasSuffix(line, "\r") {
			b.WriteByte('\r')
		}
	}
	return Result{Text: b.String(), Counts: counts}, true
}

// walk redacts string values in place. key is the nearest object key above
// the value; array elements inherit it ("api_keys": ["…"]).
func walk(n *node, key string, counts Counts) {
	switch n.kind {
	case 'o':
		for i, k := range n.keys {
			walk(n.values[i], k, counts)
		}
	case 'a':
		for _, v := range n.values {
			walk(v, key, counts)
		}
	case 's':
		n.str = redactValue(key, n.str, counts)
	}
}

// redactValue redacts one string value according to the key it sits under.
func redactValue(key, v string, counts Counts) string {
	switch {
	case preservedKey(key):
		return v
	case urlKey(key):
		return apply(v, append(credentialedURISpans(v), queryParamSpans(v)...), counts).Text
	case credentialKey(key):
		s, e := 0, len(v)
		if loc := authScheme.FindStringIndex(v); loc != nil {
			s = loc[1]
		}
		secret := v[s:e]
		if len(secret) < 4 || heldOut(secret) || shortNumber.MatchString(secret) {
			return flat(v).merge(counts)
		}
		counts[CredentialValue]++
		return v[:s] + Placeholder
	default:
		return flat(v).merge(counts)
	}
}

func (r Result) merge(into Counts) string {
	into.Add(r.Counts)
	return r.Text
}

// preservedKey reports keys whose values are ids or paths, kept verbatim.
func preservedKey(key string) bool {
	segs := keySegments(key)
	if len(segs) == 0 {
		return false
	}
	last := segs[len(segs)-1]
	switch last {
	case "id", "ids", "uuid", "path", "paths", "cwd", "dir", "directory", "file", "filename",
		"sha", "commit", "oid", "hash":
		return !credentialKey(key)
	}
	return false
}

// urlKey reports keys whose values are URLs: only credential material inside
// them is redacted.
func urlKey(key string) bool {
	segs := keySegments(key)
	if len(segs) == 0 {
		return false
	}
	switch segs[len(segs)-1] {
	case "url", "uri", "urls", "uris", "href", "link":
		return true
	}
	return false
}

// parseContainer parses s as exactly one JSON object or array.
func parseContainer(s string) (*node, bool) {
	if !json.Valid([]byte(s)) {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil || (n.kind != 'o' && n.kind != 'a') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return n, true
}

func parseValue(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key %v", kt)
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.values = append(n.values, v)
			}
			_, err := dec.Token() // '}'
			return n, err
		case '[':
			n := &node{kind: 'a'}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.values = append(n.values, v)
			}
			_, err := dec.Token() // ']'
			return n, err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return &node{kind: 's', str: t}, nil
	case json.Number:
		return &node{kind: 'n', str: t.String()}, nil
	case bool:
		return &node{kind: 'b', b: t}, nil
	case nil:
		return &node{kind: 'z'}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// encode writes n compactly. Strings are escaped the way serde_json does:
// only '"', '\\' and control characters; no HTML escaping, no  .
func encode(b *strings.Builder, n *node) {
	switch n.kind {
	case 'o':
		b.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeString(b, k)
			b.WriteByte(':')
			encode(b, n.values[i])
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, v := range n.values {
			if i > 0 {
				b.WriteByte(',')
			}
			encode(b, v)
		}
		b.WriteByte(']')
	case 's':
		encodeString(b, n.str)
	case 'n':
		b.WriteString(n.str)
	case 'b':
		if n.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case 'z':
		b.WriteString("null")
	}
}

func encodeString(b *strings.Builder, s string) {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&buf, `\u%04x`, r)
			} else {
				var tmp [utf8.UTFMax]byte
				buf.Write(tmp[:utf8.EncodeRune(tmp[:], r)])
			}
		}
	}
	buf.WriteByte('"')
	b.Write(buf.Bytes())
}
