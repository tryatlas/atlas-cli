package record

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/redact"
)

// Clean strips zero-width and bidi controls and the Unicode tag block, and
// defangs the <atlas-memory envelope (SPEC §5.3).
func Clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064,
			r >= 0x2066 && r <= 0x2069, r == 0xFEFF, r >= 0xE0000 && r <= 0xE007F:
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	out = strings.ReplaceAll(out, "</atlas-memory", "‹/atlas-memory")
	return strings.ReplaceAll(out, "<atlas-memory", "‹atlas-memory")
}

// RedactText is what every stored memory text goes through: Clean, then the
// flat redactor.
func RedactText(s string) (string, error) {
	r, err := redact.Flat(Clean(s))
	if err != nil {
		return "", err
	}
	return r.Text, nil
}

// Normalize collapses whitespace to single spaces and lower-cases.
func Normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ContentHash is lowercase hex sha256 of Normalize(text).
func ContentHash(s string) string {
	sum := sha256.Sum256([]byte(Normalize(s)))
	return hex.EncodeToString(sum[:])
}
