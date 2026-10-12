package embed

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMatchesTheAppsVectors embeds entries of a real app-written record and
// compares them with the vectors the desktop cached for the same text. It
// runs only where both exist (ATLAS_COMPAT_RECORD names the memory.sqlite).
func TestMatchesTheAppsVectors(t *testing.T) {
	path := os.Getenv("ATLAS_COMPAT_RECORD")
	if path == "" || !Available() {
		t.Skip("set ATLAS_COMPAT_RECORD to an app-written memory.sqlite; needs the model")
	}
	e, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT content FROM entries ORDER BY id DESC LIMIT 60`)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		contents = append(contents, c)
	}
	_ = rows.Close()
	matched, worst := 0, float32(1)
	start := time.Now()
	for _, c := range contents {
		var vec []byte
		if db.QueryRow(`SELECT vec FROM embed_cache WHERE key = ?`, CacheKey(ModelName, c)).Scan(&vec) != nil {
			continue
		}
		matched++
		cos := Cosine(e.Embed(c), Decode(vec))
		worst = min(worst, cos)
		if cos < 0.999 {
			t.Errorf("cosine %.5f for %q", cos, c)
		}
		// Our f16 bytes should be nearly identical to the app's.
		if b := Encode(e.Embed(c)); len(b) != len(vec) {
			t.Fatalf("encoded %d bytes, app %d", len(b), len(vec))
		}
	}
	if matched == 0 {
		t.Fatal("no entry's key matched the app's cache: the key derivation differs")
	}
	t.Logf("%d entries matched the app's cache keys; worst cosine %.6f; %s", matched, worst, time.Since(start))
}

func TestF16RoundTrip(t *testing.T) {
	for _, x := range []float32{0, 1, -1, 0.5, 0.1, -0.333, 65504, 1e-5, 6e-8, 3.14159} {
		got := f16to32(f32to16(x))
		if d := got - x; d > 0.001*max(1, x) || d < -0.001*max(1, -x) {
			t.Errorf("%v → %v", x, got)
		}
	}
	if f32to16(1) != 0x3c00 || f32to16(-2) != 0xc000 || f32to16(0.1) != 0x2e66 {
		t.Fatalf("%x %x %x", f32to16(1), f32to16(-2), f32to16(0.1))
	}
}
