package chat

import (
	"strings"
	"testing"
)

func render(ls []Line) string {
	var b []string
	for _, l := range ls {
		b = append(b, string(l.Op)+l.Text)
	}
	return strings.Join(b, "\n")
}

func TestDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\n"
	got := render(Diff(&old, "a\nb\nc\nD\ne\nf\ng\n", 1))
	want := "@2 unchanged lines\n c\n-d\n+D\n e\n@2 unchanged lines"
	if got != want {
		t.Fatalf("\n%s\nwant\n%s", got, want)
	}
	if got := render(Diff(nil, "x\ny\n", 3)); got != "+x\n+y" {
		t.Fatalf("new file: %q", got)
	}
	empty := ""
	if got := render(Diff(&empty, "x", 3)); got != "+x" {
		t.Fatalf("%q", got)
	}
	o2 := "keep\nold\n"
	if got := render(Diff(&o2, "keep\n", 3)); got != " keep\n-old" {
		t.Fatalf("%q", got)
	}
}
