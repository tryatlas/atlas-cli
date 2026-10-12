package tui

import (
	"strings"
)

// The Atlas mark is "///" set in Array Bold (Fontshare), as in the desktop
// app's src/assets/atlas-icon.svg. Array's "/" is a stripe of round dots 4
// wide and 14 tall, moving one column right every two rows; the logo sets
// three of them 3 dot columns apart. Here every dot column is kept and two
// dot rows share a text row (a terminal cell is about twice as tall as it
// is wide), which keeps the glyph's slope and the logo's proportions. Drawn
// in the logo's cream; animations only switch dots between the cream and a
// dim grey, never through a gradient.

const (
	glyphRows  = 14 // dot rows of Array Bold's "/"
	glyphWidth = 4  // dots across one stripe
	glyphGap   = 3  // dot columns between stripes in the logo
	logoCols   = 3*glyphWidth + 2*glyphGap + glyphRows/2 - 1
)

// Logo is the mark, 7 rows by 24 columns of dots. Stripes light one by one,
// each from the bottom up, as t goes from 0 to 1 (1: all lit).
func Logo(t float64) string {
	const rows = glyphRows / 2
	var b strings.Builder
	for r := range rows {
		off := rows - 1 - r // one column right per text row (two dot rows)
		line := []rune(strings.Repeat(" ", logoCols))
		lit := make([]bool, logoCols)
		for s := range 3 {
			x := off + s*(glyphWidth+glyphGap)
			on := (float64(s)+float64(rows-1-r)/float64(rows))/3 < t
			for i := range glyphWidth {
				line[x+i] = '●'
				lit[x+i] = on
			}
		}
		for i, c := range line {
			switch {
			case c == ' ':
				b.WriteByte(' ')
			case lit[i]:
				b.WriteString(Default.Logo.Render("●"))
			default:
				b.WriteString(Default.LogoDim.Render("·"))
			}
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// stripe is one stripe of the working pulse: two block quadrants that read
// as a slanted bar in any font.
const stripe = "▟▛"

// Wordmark is the name that heads every screen; the logo itself is kept
// for the splash and the init intro, where it has room.
func Wordmark() string { return Default.Bold.Render("Atlas") }

// pulse is a frame of the working mark: one stripe lit, the one behind it
// half lit, the rest dim. Monochrome: cream, grey, dark grey.
func pulse(frame int) string {
	const n = 5 // three stripes, then two dark beats
	lead := frame % n
	parts := make([]string, 3)
	for s := range 3 {
		switch lead - s {
		case 0:
			parts[s] = Default.Logo.Render(stripe)
		case 1:
			parts[s] = Default.Secondary.Render(stripe)
		default:
			parts[s] = Default.LogoDim.Render(stripe)
		}
	}
	return strings.Join(parts, " ")
}
