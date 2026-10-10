// Package timefmt writes timestamps the way the desktop's chrono
// `DateTime<Utc>::to_rfc3339()` does: UTC, a "+00:00" suffix (never "Z"), and
// fractional seconds only when non-zero, in groups of 3, 6 or 9 digits. The
// capture store compares these strings lexically, so the format is load-bearing
// (evidence-capture.md §3.4).
package timefmt

import (
	"fmt"
	"time"
)

// RFC3339 formats t as chrono's to_rfc3339 for a UTC time.
func RFC3339(t time.Time) string {
	t = t.UTC()
	base := t.Format("2006-01-02T15:04:05")
	ns := t.Nanosecond()
	switch {
	case ns == 0:
		return base + "+00:00"
	case ns%1_000_000 == 0:
		return fmt.Sprintf("%s.%03d+00:00", base, ns/1_000_000)
	case ns%1_000 == 0:
		return fmt.Sprintf("%s.%06d+00:00", base, ns/1_000)
	default:
		return fmt.Sprintf("%s.%09d+00:00", base, ns)
	}
}

// Parse reads an RFC 3339 timestamp (either suffix). Like the desktop, an
// unparseable value reads as the Unix epoch.
func Parse(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return t.UTC()
}
