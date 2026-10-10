package redact

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

// Over-redaction is a failure too (redact-README.md): a false positive is
// permanent, silent data loss. These are the values every layer leaves alone.

var (
	envRef       = regexp.MustCompile(`^(?:\$[A-Za-z_][A-Za-z0-9_]*|\$\{[^{}\s]+\}|%[A-Za-z_][A-Za-z0-9_]*%|\$\(\s*[^()]+\))$`)
	templateRef  = regexp.MustCompile(`^(?:\{\{.*\}\}|\$\{\{.*\}\}|<[^<>]{1,80}>|\[[A-Z_ -]{3,40}\]|@[A-Za-z_]+@)$`)
	placeholderW = regexp.MustCompile(`(?i)(?:example|placeholder|changeme|change[_-]me|dummy|redacted|your[_-]?(?:api|secret|access|auth|own|real)?[_-]?(?:key|token|secret|password|pass)|insert[_-]|replace[_-]?me|not[_-]?a[_-]?(?:real|secret)|xxxxx)`)
	maskRun      = regexp.MustCompile(`\*{3,}|•{3,}|x{6,}|X{6,}|\.{3,}|…`)
	literalWord  = regexp.MustCompile(`(?i)^(?:true|false|null|nil|none|undefined|empty|unset|required|optional|secret|password|passwd|pass|pwd|token|key|str|string|int|bool|bytes|any|unknown|object|number|text|hidden|masked|set|on|off|yes|no)$`)
	typeName     = regexp.MustCompile(`^(?:&?str|&'static str|String|Option<[^>]+>|Optional\[[^\]]+\]|\*?string|\[\]byte|SecretStr|SecretString|Secret<[^>]+>)$`)
	pureHex      = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	uuidShape    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	dottedIdent  = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+$`)
	shortNumber  = regexp.MustCompile(`^[0-9]{1,5}$`)
	numberRange  = regexp.MustCompile(`^[0-9]+(?:[-,:][0-9]+)+$`)
	placeholderS = []string{Placeholder, "[redacted]", "<redacted>", "REDACTED"}
)

// heldOut reports whether a candidate secret value must not be redacted,
// whichever layer found it.
func heldOut(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	for _, p := range placeholderS {
		if strings.Contains(v, p) {
			return true
		}
	}
	return envRef.MatchString(v) ||
		templateRef.MatchString(v) ||
		placeholderW.MatchString(v) ||
		maskRun.MatchString(v) ||
		literalWord.MatchString(v) ||
		typeName.MatchString(v) ||
		uuidShape.MatchString(v) ||
		isRepeat(v)
}

// isRepeat reports a value made of one repeated character ("aaaa", "0000").
func isRepeat(v string) bool {
	r := []rune(v)
	for _, c := range r[1:] {
		if c != r[0] {
			return false
		}
	}
	return len(r) > 1
}

// isGitSHA reports an abbreviated or full git object id, or a sha256 digest.
func isGitSHA(v string) bool {
	n := len(v)
	return pureHex.MatchString(v) && ((n >= 7 && n <= 40) || n == 64)
}

// shannon is the Shannon entropy of s in bits per character.
func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]int{}
	n := 0
	for _, r := range s {
		freq[r]++
		n++
	}
	var h float64
	for _, c := range freq {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

// wordy reports text built from words: camelCase, snake_case or kebab-case
// identifiers, version strings. Its character-class runs are long, where a
// random token's are short.
func wordy(s string) bool {
	runs, n := 0, 0
	var prev rune
	for i, r := range s {
		if r == '-' || r == '_' || r == '/' || r == '.' {
			prev = 0 // a separator ends a run and is not counted itself
			continue
		}
		n++
		cls := class(r)
		if prev == 0 {
			runs++
			prev = cls
			continue
		}
		// An upper-case letter followed by lower case is one Capitalised word.
		capitalised := prev == 'U' && cls == 'l' && runLen(s, i) == 1
		if cls != prev && !capitalised {
			runs++
		}
		prev = cls
	}
	return runs > 0 && float64(n)/float64(runs) >= 3.0
}

// runLen is the length of the run of upper-case letters ending just before byte i.
func runLen(s string, i int) int {
	n := 0
	for j := i - 1; j >= 0 && unicode.IsUpper(rune(s[j])); j-- {
		n++
	}
	return n
}

func class(r rune) rune {
	switch {
	case unicode.IsLower(r):
		return 'l'
	case unicode.IsUpper(r):
		return 'U'
	case unicode.IsDigit(r):
		return 'd'
	default:
		return 's'
	}
}
