// Command genrules turns the betterleaks default configuration into the
// redactor's vendored rule table (internal/redact/vendor_rules.json).
//
//	go run ./tools/genrules -config betterleaks.toml -source betterleaks/betterleaks@<sha> \
//	    -out internal/redact/vendor_rules.json
//
// A rule is kept when it has a regex, is not path-scoped, is not composite,
// and its filter is a disjunction of clauses the redactor can evaluate on the
// matched value alone: an entropy bound, matchesAny / containsAny on the
// secret. Clauses on the file path are dropped (the redactor has no path).
// Every other rule is listed in "excluded" with the reason, so a dropped rule
// is a recorded decision rather than a silent gap.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type config struct {
	Filter string `toml:"filter"`
	Rules  []rule `toml:"rules"`
}

type rule struct {
	ID         string           `toml:"id"`
	Regex      string           `toml:"regex"`
	Path       string           `toml:"path"`
	Keywords   []string         `toml:"keywords"`
	Filter     string           `toml:"filter"`
	ValueGroup int              `toml:"valueGroup"`
	Components []map[string]any `toml:"components"`
}

// Filter is the part of a betterleaks filter the redactor evaluates: a
// candidate is discarded when any clause holds.
type Filter struct {
	EntropyBelow     float64  `json:"entropyBelow,omitempty"`
	EntropyInclusive bool     `json:"entropyInclusive,omitempty"`
	SecretMatches    []string `json:"secretMatches,omitempty"`
	SecretContains   []string `json:"secretContains,omitempty"`
	// SecretMustMatch discards a candidate that matches none of these
	// (betterleaks `!matchesAny(finding["secret"], …)`).
	SecretMustMatch []string `json:"secretMustMatch,omitempty"`
}

// Rule is one vendored detection rule.
type Rule struct {
	ID         string   `json:"id"`
	Regex      string   `json:"regex"`
	Keywords   []string `json:"keywords"`
	ValueGroup int      `json:"valueGroup,omitempty"`
	Filter     *Filter  `json:"filter,omitempty"`
}

// Excluded records a rule left out and why.
type Excluded struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Table is the generated file.
type Table struct {
	Source       string     `json:"source"`
	License      string     `json:"license"`
	GlobalFilter Filter     `json:"globalFilter"`
	Rules        []Rule     `json:"rules"`
	Excluded     []Excluded `json:"excluded"`
}

func main() {
	cfgPath := flag.String("config", "", "path to betterleaks.toml")
	source := flag.String("source", "", "provenance, e.g. betterleaks/betterleaks@<sha>")
	out := flag.String("out", "", "output JSON path")
	flag.Parse()
	if *cfgPath == "" || *source == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := generate(*cfgPath, *source, *out); err != nil {
		fmt.Fprintln(os.Stderr, "genrules:", err)
		os.Exit(1)
	}
}

func generate(cfgPath, source, out string) error {
	raw, err := os.ReadFile(cfgPath) //nolint:gosec // G304: a path the developer passes on purpose
	if err != nil {
		return err
	}
	var cfg config
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	global, err := parseFilter(cfg.Filter)
	if err != nil {
		return fmt.Errorf("global filter: %w", err)
	}
	if global.EntropyBelow != 0 {
		return fmt.Errorf("global filter: unexpected entropy clause")
	}
	t := Table{Source: source, License: "MIT, (c) 2026 Zachary Rice; see LICENSE-betterleaks", GlobalFilter: *global}
	for _, r := range cfg.Rules {
		kept, reason := convert(r)
		if kept == nil {
			t.Excluded = append(t.Excluded, Excluded{ID: r.ID, Reason: reason})
			continue
		}
		t.Rules = append(t.Rules, *kept)
	}
	sort.Slice(t.Rules, func(i, j int) bool { return t.Rules[i].ID < t.Rules[j].ID })
	sort.Slice(t.Excluded, func(i, j int) bool { return t.Excluded[i].ID < t.Excluded[j].ID })
	buf, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "genrules: %d rules kept, %d excluded\n", len(t.Rules), len(t.Excluded))
	return os.WriteFile(out, append(buf, '\n'), 0o644) //nolint:gosec // G306: generated source file, not a secret
}

func convert(r rule) (*Rule, string) {
	switch {
	case r.Regex == "":
		return nil, "path-only rule"
	case r.Path != "":
		return nil, "path-scoped rule"
	case len(r.Components) > 0:
		return nil, "composite rule (needs other findings)"
	case len(r.Keywords) == 0:
		return nil, "no keywords for the prefilter"
	}
	if _, err := regexp.Compile(r.Regex); err != nil {
		return nil, "regex does not compile under Go RE2: " + err.Error()
	}
	f, err := parseFilter(r.Filter)
	if err != nil {
		return nil, "filter: " + err.Error()
	}
	kws := make([]string, len(r.Keywords))
	for i, k := range r.Keywords {
		kws[i] = strings.ToLower(k)
	}
	out := &Rule{ID: r.ID, Regex: r.Regex, Keywords: kws, ValueGroup: r.ValueGroup}
	if !f.empty() {
		out.Filter = f
	}
	return out, ""
}

func (f *Filter) empty() bool {
	return f.EntropyBelow == 0 && len(f.SecretMatches) == 0 && len(f.SecretContains) == 0 && len(f.SecretMustMatch) == 0
}

var (
	entropyClause  = regexp.MustCompile(`^entropy\(finding\["secret"\]\)\s*(<=|<)\s*([0-9.]+)$`)
	secretMatches  = regexp.MustCompile(`^matchesAny\(finding\["secret"\],\s*\[([\s\S]*)\]\)$`)
	secretNotMatch = regexp.MustCompile(`^!matchesAny\(finding\["secret"\],\s*\[([\s\S]*)\]\)$`)
	secretContains = regexp.MustCompile(`^containsAny\(finding\["secret"\],\s*\[([\s\S]*)\]\)$`)
	pathMatches    = regexp.MustCompile(`^matchesAny\(attributes\["path"\],\s*\[[\s\S]*\]\)$`)
	listItem       = regexp.MustCompile("`([^`]*)`|\"((?:[^\"\\\\]|\\\\.)*)\"")
)

// parseFilter reads a betterleaks filter expression that is a disjunction of
// supported clauses. Anything else is an error, and the rule is excluded.
func parseFilter(expr string) (*Filter, error) {
	f := &Filter{}
	expr = stripComments(expr)
	if strings.TrimSpace(expr) == "" {
		return f, nil
	}
	for _, clause := range splitTop(expr, "||") {
		c := unwrap(strings.TrimSpace(clause))
		switch {
		case entropyClause.MatchString(c):
			m := entropyClause.FindStringSubmatch(c)
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				return nil, err
			}
			if f.EntropyBelow != 0 {
				return nil, fmt.Errorf("two entropy clauses")
			}
			f.EntropyBelow, f.EntropyInclusive = v, m[1] == "<="
		case secretMatches.MatchString(c):
			items, err := compiledItems(secretMatches.FindStringSubmatch(c)[1])
			if err != nil {
				return nil, err
			}
			f.SecretMatches = append(f.SecretMatches, items...)
		case secretNotMatch.MatchString(c):
			items, err := compiledItems(secretNotMatch.FindStringSubmatch(c)[1])
			if err != nil {
				return nil, err
			}
			f.SecretMustMatch = append(f.SecretMustMatch, items...)
		case secretContains.MatchString(c):
			items, err := listItems(secretContains.FindStringSubmatch(c)[1])
			if err != nil {
				return nil, err
			}
			f.SecretContains = append(f.SecretContains, items...)
		case pathMatches.MatchString(c):
			// The redactor sees text, not files: a path exclusion never applies.
		default:
			return nil, fmt.Errorf("unsupported clause %q", firstLine(c))
		}
	}
	return f, nil
}

// compiledItems reads a list of regexes and checks each compiles under Go RE2.
func compiledItems(body string) ([]string, error) {
	items, err := listItems(body)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if _, err := regexp.Compile(it); err != nil {
			return nil, fmt.Errorf("secret filter %q does not compile: %w", it, err)
		}
	}
	return items, nil
}

func listItems(body string) ([]string, error) {
	var out []string
	for _, m := range listItem.FindAllStringSubmatch(body, -1) {
		if m[1] != "" || strings.HasPrefix(m[0], "`") {
			out = append(out, m[1])
			continue
		}
		s, err := strconv.Unquote(`"` + m[2] + `"`)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func stripComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// splitTop splits on sep outside brackets, parentheses, braces and quotes.
func splitTop(s, sep string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '`', '"':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if depth == 0 && strings.HasPrefix(s[i:], sep) {
				parts = append(parts, s[start:i])
				start = i + len(sep)
				i += len(sep) - 1
			}
		}
	}
	return append(parts, s[start:])
}

// unwrap removes parentheses that enclose a whole clause.
func unwrap(s string) string {
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		inner := s[1 : len(s)-1]
		if !balanced(inner) {
			return s
		}
		s = strings.TrimSpace(inner)
	}
	return s
}

func balanced(s string) bool {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '`', '"':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
