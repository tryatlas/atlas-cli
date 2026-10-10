package timefmt

import (
	"testing"
	"time"
)

func TestRFC3339(t *testing.T) {
	base := time.Date(2026, 10, 10, 12, 34, 56, 0, time.UTC)
	cases := []struct {
		ns   int
		want string
	}{
		{0, "2026-10-10T12:34:56+00:00"},
		{789_000_000, "2026-10-10T12:34:56.789+00:00"},
		{789_012_000, "2026-10-10T12:34:56.789012+00:00"},
		{789_012_345, "2026-10-10T12:34:56.789012345+00:00"},
		{1_000, "2026-10-10T12:34:56.000001+00:00"},
	}
	for _, c := range cases {
		if got := RFC3339(base.Add(time.Duration(c.ns))); got != c.want {
			t.Errorf("%d ns: got %q, want %q", c.ns, got, c.want)
		}
	}
	east := time.FixedZone("x", 6*3600)
	if got := RFC3339(time.Date(2026, 10, 10, 18, 0, 0, 0, east)); got != "2026-10-10T12:00:00+00:00" {
		t.Errorf("non-UTC input: %q", got)
	}
}

func TestParse(t *testing.T) {
	if got := Parse("2026-07-22T00:00:00Z"); !got.Equal(time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Z suffix: %v", got)
	}
	if got := Parse("garbage"); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("garbage: %v", got)
	}
}
