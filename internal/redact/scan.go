package redact

// asciiLower lower-cases ASCII letters byte for byte, so offsets into the
// result are offsets into the original.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// matcher finds which of a fixed set of keywords occur in a text, in one
// pass (Aho–Corasick over bytes).
type matcher struct {
	next [][256]int32 // goto/fail-resolved transitions
	out  [][]int      // keyword indexes ending at each state
}

func newMatcher(keywords []string) *matcher {
	m := &matcher{next: make([][256]int32, 1), out: make([][]int, 1)}
	for i := range m.next[0] {
		m.next[0][i] = -1
	}
	for k, kw := range keywords {
		s := 0
		for i := 0; i < len(kw); i++ {
			c := kw[i]
			if m.next[s][c] < 0 {
				var row [256]int32
				for j := range row {
					row[j] = -1
				}
				m.next = append(m.next, row)
				m.out = append(m.out, nil)
				m.next[s][c] = int32(len(m.next) - 1) //nolint:gosec // G115: states ≤ total keyword bytes, far below 2^31
			}
			s = int(m.next[s][c])
		}
		m.out[s] = append(m.out[s], k)
	}
	// Breadth-first: resolve failure links into the transition table.
	fail := make([]int, len(m.next))
	queue := make([]int, 0, len(m.next))
	for c := 0; c < 256; c++ {
		if t := m.next[0][c]; t < 0 {
			m.next[0][c] = 0
		} else {
			fail[t] = 0
			queue = append(queue, int(t))
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		m.out[s] = append(m.out[s], m.out[fail[s]]...)
		for c := 0; c < 256; c++ {
			t := m.next[s][c]
			if t < 0 {
				m.next[s][c] = m.next[fail[s]][c]
				continue
			}
			fail[t] = int(m.next[fail[s]][c])
			queue = append(queue, int(t))
		}
	}
	return m
}

// present marks seen[k] for every keyword k that occurs in text.
func (m *matcher) present(text string, seen []bool) {
	s := int32(0)
	for i := 0; i < len(text); i++ {
		s = m.next[s][text[i]]
		for _, k := range m.out[s] {
			seen[k] = true
		}
	}
}
