package record

import (
	"context"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/cli/internal/embed"
)

// bagOfWords is a deterministic stand-in for the model: a hashed bag of
// lowercase words, unit length. Same words, same vector.
type bagOfWords struct{}

func (bagOfWords) Embed(text string) []float32 {
	v := make([]float32, 64)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(w, ".,")))
		v[h.Sum32()%64]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(n))
	}
	return v
}

func withVectors(t *testing.T, src VectorSource) {
	old := Vectors
	Vectors = func() VectorSource { return src }
	t.Cleanup(func() { Vectors = old })
}

func TestRememberCachesVectorsAndMergesNearDuplicates(t *testing.T) {
	withVectors(t, bagOfWords{})
	ctx := context.Background()
	st, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UnixMilli()
	w := Write{Kind: "decision", Content: "Use Postgres for the ledger service", Source: "agent", Agent: "pi", Session: "s1", Confidence: 0.8, At: at}
	if o, _, err := st.Remember(ctx, w); err != nil || o != Inserted {
		t.Fatalf("%s %v", o, err)
	}
	if cachedVector(ctx, st.db, w.Content) == nil {
		t.Fatal("a durable write caches its vector")
	}
	// The same words in another order: a near-duplicate, merged.
	w2 := w
	w2.Content, w2.Session, w2.At = "For the ledger service use Postgres", "s2", at+1
	o, e, err := st.Remember(ctx, w2)
	if err != nil || o != Merged || e.Uses != 1 {
		t.Fatalf("near-duplicate: %s %+v %v", o, e, err)
	}
	// A contradicting text is stored, never merged.
	w3 := w
	w3.Content, w3.Session, w3.At = "Do not use Postgres for the ledger service", "s3", at+2
	if o, _, _ := st.Remember(ctx, w3); o != Inserted {
		t.Fatalf("contradiction merged: %s", o)
	}
	// Plans and file changes are not embedded.
	if _, _, err := st.Remember(ctx, Write{Kind: "plan", Content: "Step one", Source: "agent", Agent: "pi", Session: "s1", At: at}); err != nil {
		t.Fatal(err)
	}
	if cachedVector(ctx, st.db, "Step one") != nil {
		t.Fatal("a plan has no vector")
	}
}

func TestSearchFusesTheMeaningLeg(t *testing.T) {
	withVectors(t, bagOfWords{})
	ctx := context.Background()
	st, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UnixMilli()
	for i, c := range []string{"Ledger writes go through Postgres", "The cache is Redis", "Deploys run on Fridays"} {
		if _, _, err := st.Remember(ctx, Write{Kind: "fact", Content: c, Source: "agent", Agent: "pi", Session: "s", Confidence: 0.7, At: at + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := st.Search(ctx, "ledger postgres", nil, 10)
	if err != nil || len(hits) == 0 || hits[0].Entry.Content != "Ledger writes go through Postgres" || hits[0].BM25 != 1 || hits[0].Dense != 1 {
		t.Fatalf("%+v %v", hits, err)
	}
	// Without a model: keyword only, same top hit, no dense rank.
	withVectors(t, nil)
	hits, _ = st.Search(ctx, "ledger postgres", nil, 10)
	if len(hits) == 0 || hits[0].Dense != 0 {
		t.Fatalf("%+v", hits)
	}
}

func TestSyncVectorsBackfillsAndCollectsGarbage(t *testing.T) {
	withVectors(t, nil)
	ctx := context.Background()
	st, _ := Open(t.TempDir(), true)
	defer st.Close()
	_, _, _ = st.Remember(ctx, Write{Kind: "fact", Content: "Builds use Go 1.26", Source: "agent", Agent: "pi", Session: "s", At: 1})
	_ = putVector(ctx, st.db, "a text no entry has", []float32{1, 0})
	withVectors(t, bagOfWords{})
	n, err := st.SyncVectors(ctx)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if cachedVector(ctx, st.db, "Builds use Go 1.26") == nil || cachedVector(ctx, st.db, "a text no entry has") != nil {
		t.Fatal("backfill and garbage")
	}
}

func TestContradictsText(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Timeout is 30s", "Timeout is 60s", true},
		{"We use JWT", "We don't use JWT", true},
		{"We use JWT", "JWT is what we use", false},
		{"Retry 3 times", "Retry 3 times.", false},
	} {
		if got := ContradictsText(c.a, c.b); got != c.want {
			t.Errorf("%q vs %q = %v", c.a, c.b, got)
		}
	}
}

// TestRealModelFindsMeaning runs only where the app downloaded the model.
func TestRealModelFindsMeaning(t *testing.T) {
	e, err := embed.Load(filepath.Join(embed.AppDataDir(), "models", embed.ModelID))
	if err != nil {
		t.Skip("the model is not on this machine")
	}
	withVectors(t, e)
	ctx := context.Background()
	st, _ := Open(t.TempDir(), true)
	defer st.Close()
	at := time.Now().UnixMilli()
	for i, c := range []string{"Signing keys for access tokens rotate every week", "The UI uses a monochrome palette", "CI runs golangci-lint before tests"} {
		if _, _, err := st.Remember(ctx, Write{Kind: "decision", Content: c, Source: "agent", Agent: "pi", Session: "s", Confidence: 0.8, At: at + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := st.Search(ctx, "how often do we change JWT secrets", nil, 3)
	if err != nil || len(hits) == 0 || !strings.HasPrefix(hits[0].Entry.Content, "Signing keys") || hits[0].Dense != 1 {
		t.Fatalf("%+v %v", hits, err)
	}
}
