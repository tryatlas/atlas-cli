package chat

import (
	"strconv"
	"strings"
)

// Line is one line of a rendered diff: ' ' context, '+' added, '-' removed,
// '@' a skipped stretch.
type Line struct {
	Op   byte
	Text string
}

// maxDiffLines bounds the LCS table; larger diffs show removed then added.
const maxDiffLines = 600

// Diff is a line diff of a diff content block (oldText nil = a new file),
// with context lines around each change.
func Diff(oldText *string, newText string, context int) []Line {
	var a []string
	if oldText != nil && *oldText != "" {
		a = splitLines(*oldText)
	}
	b := splitLines(newText)
	var ops []Line
	if len(a)*len(b) > maxDiffLines*maxDiffLines {
		for _, l := range a {
			ops = append(ops, Line{'-', l})
		}
		for _, l := range b {
			ops = append(ops, Line{'+', l})
		}
		return ops
	}
	// LCS table, then walk it.
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, Line{' ', a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, Line{'-', a[i]})
			i++
		default:
			ops = append(ops, Line{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, Line{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, Line{'+', b[j]})
	}
	return trimContext(ops, context)
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// trimContext keeps n unchanged lines around each change and folds the
// rest into one '@' line.
func trimContext(ops []Line, n int) []Line {
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.Op == ' ' {
			continue
		}
		for k := max(0, i-n); k <= min(len(ops)-1, i+n); k++ {
			keep[k] = true
		}
	}
	var out []Line
	skipped := 0
	for i, op := range ops {
		if keep[i] {
			if skipped > 0 {
				out = append(out, Line{'@', skippedText(skipped)})
				skipped = 0
			}
			out = append(out, op)
			continue
		}
		skipped++
	}
	if skipped > 0 && len(out) > 0 {
		out = append(out, Line{'@', skippedText(skipped)})
	}
	return out
}

func skippedText(n int) string {
	if n == 1 {
		return "1 unchanged line"
	}
	return strconv.Itoa(n) + " unchanged lines"
}
