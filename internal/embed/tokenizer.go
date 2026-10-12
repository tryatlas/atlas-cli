package embed

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Tokenizer is the BERT WordPiece tokenizer of a tokenizer.json, as the
// Hugging Face tokenizers crate runs it for all-MiniLM-L6-v2:
// BertNormalizer (clean text, space out CJK, strip accents, lowercase),
// BertPreTokenizer (whitespace and punctuation), WordPiece ("##", [UNK],
// 100 chars a word), then [CLS] … [SEP], truncated to MaxTokens.
type Tokenizer struct {
	vocab    map[string]int32
	unk      int32
	cls, sep int32
	special  []string // added tokens, matched on the raw text first
	maxWord  int
	prefix   string
}

// MaxTokens is the desktop's truncation (atlas-embed spec max_tokens),
// [CLS] and [SEP] included; tokenizer.json's own 128 is overridden.
const MaxTokens = 512

// LoadTokenizer reads tokenizer.json.
func LoadTokenizer(path string) (*Tokenizer, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the model's own file
	if err != nil {
		return nil, err
	}
	var tj struct {
		Model struct {
			Type    string           `json:"type"`
			Vocab   map[string]int32 `json:"vocab"`
			Unk     string           `json:"unk_token"`
			Prefix  string           `json:"continuing_subword_prefix"`
			MaxWord int              `json:"max_input_chars_per_word"`
		} `json:"model"`
		Added []struct {
			Content string `json:"content"`
		} `json:"added_tokens"`
	}
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if tj.Model.Type != "WordPiece" || len(tj.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer.json: not a WordPiece tokenizer")
	}
	t := &Tokenizer{vocab: tj.Model.Vocab, prefix: tj.Model.Prefix, maxWord: tj.Model.MaxWord}
	if t.prefix == "" {
		t.prefix = "##"
	}
	if t.maxWord == 0 {
		t.maxWord = 100
	}
	var ok [3]bool
	t.unk, ok[0] = t.vocab[orDefault(tj.Model.Unk, "[UNK]")]
	t.cls, ok[1] = t.vocab["[CLS]"]
	t.sep, ok[2] = t.vocab["[SEP]"]
	if !ok[0] || !ok[1] || !ok[2] {
		return nil, fmt.Errorf("tokenizer.json: missing [UNK], [CLS] or [SEP]")
	}
	for _, a := range tj.Added {
		t.special = append(t.special, a.Content)
	}
	return t, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Encode returns the token ids of one text: [CLS] tokens [SEP].
func (t *Tokenizer) Encode(text string) []int32 {
	ids := []int32{t.cls}
	for _, seg := range t.splitSpecial(text) {
		if seg.special {
			if id, ok := t.vocab[seg.text]; ok {
				ids = append(ids, id)
			}
			continue
		}
		for _, w := range preTokenize(normalize(seg.text)) {
			ids = append(ids, t.wordPiece(w)...)
		}
	}
	if len(ids) > MaxTokens-1 {
		ids = ids[:MaxTokens-1]
	}
	return append(ids, t.sep)
}

type segment struct {
	text    string
	special bool
}

// splitSpecial cuts out the added tokens ([CLS], [SEP], …) written in the
// text itself; tokenizers matches them before normalizing.
func (t *Tokenizer) splitSpecial(text string) []segment {
	var out []segment
	for len(text) > 0 {
		at, which := -1, ""
		for _, s := range t.special {
			if i := strings.Index(text, s); i >= 0 && (at < 0 || i < at || i == at && len(s) > len(which)) {
				at, which = i, s
			}
		}
		if at < 0 {
			out = append(out, segment{text: text})
			break
		}
		if at > 0 {
			out = append(out, segment{text: text[:at]})
		}
		out = append(out, segment{text: which, special: true})
		text = text[at+len(which):]
	}
	return out
}

// normalize is BertNormalizer with clean_text, handle_chinese_chars,
// strip_accents (following lowercase) and lowercase.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0 || r == 0xFFFD || isControl(r):
			continue
		case isWhitespace(r):
			b.WriteByte(' ')
		case isChinese(r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	// Strip accents: NFD, then drop nonspacing marks.
	var c strings.Builder
	for _, r := range norm.NFD.String(b.String()) {
		if !unicode.Is(unicode.Mn, r) {
			c.WriteRune(r)
		}
	}
	return strings.ToLower(c.String())
}

// isWhitespace is the normalizer's: tab, newline, return, and Unicode
// White_Space (checked after control characters were dropped).
func isWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return true
	}
	return unicode.IsSpace(r)
}

// isControl is tokenizers' is_control: every "other" category except tab,
// newline and carriage return (which are whitespace).
func isControl(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
		return true
	}
	// Cn, unassigned: in no other category.
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

func isChinese(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF || r >= 0x20000 && r <= 0x2A6DF ||
		r >= 0x2A700 && r <= 0x2B73F || r >= 0x2B740 && r <= 0x2B81F || r >= 0x2B920 && r <= 0x2CEAF ||
		r >= 0xF900 && r <= 0xFAFF || r >= 0x2F800 && r <= 0x2FA1F
}

// isPunct is BERT's punctuation: ASCII punctuation, or Unicode P*.
func isPunct(r rune) bool {
	if r < 128 {
		return r >= 33 && r <= 47 || r >= 58 && r <= 64 || r >= 91 && r <= 96 || r >= 123 && r <= 126
	}
	return unicode.Is(unicode.P, r)
}

// preTokenize splits on whitespace (dropped) and punctuation (kept, one
// character a word).
func preTokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunct(r):
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// wordPiece is greedy longest-match-first; a word with a piece not in the
// vocabulary, or longer than maxWord characters, is [UNK].
func (t *Tokenizer) wordPiece(word string) []int32 {
	rs := []rune(word)
	if len(rs) > t.maxWord {
		return []int32{t.unk}
	}
	var ids []int32
	for start := 0; start < len(rs); {
		end := len(rs)
		found := int32(-1)
		for end > start {
			sub := string(rs[start:end])
			if start > 0 {
				sub = t.prefix + sub
			}
			if id, ok := t.vocab[sub]; ok {
				found = id
				break
			}
			end--
		}
		if found < 0 {
			return []int32{t.unk}
		}
		ids = append(ids, found)
		start = end
	}
	return ids
}
