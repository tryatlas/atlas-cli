// Package embed computes the desktop's memory embeddings in pure Go:
// sentence-transformers/all-MiniLM-L6-v2 (BERT, 6 layers, 384 dimensions),
// mean-pooled over the tokens and L2-normalised, as crates/atlas-embed runs
// it with candle. It reads the model files the app downloads
// (<app data>/models/all-MiniLM-L6-v2/{config.json,tokenizer.json,
// model.safetensors}); vectors are cached as the app caches them, f16
// little-endian under blake3(model ‖ 0 ‖ text), so either side reuses the
// other's.
package embed

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/zeebo/blake3"
)

// The memory model, as the desktop names it.
const (
	ModelID   = "all-MiniLM-L6-v2"
	Repo      = "sentence-transformers/all-MiniLM-L6-v2"
	Dim       = 384
	ModelName = "all-MiniLM-L6-v2-384" // provider name: "<catalog id>-<dim>"
)

// Files are the model files, in download order.
var Files = []string{"config.json", "tokenizer.json", "model.safetensors"}

// ErrNoModel says the model files are not on this machine.
var ErrNoModel = errors.New("the memory search model is not downloaded (atlas-cli memory index downloads it)")

// AppDataDir is the desktop's app data directory (Tauri app_data_dir):
// macOS ~/Library/Application Support/dev.atlas.ide, Linux
// $XDG_DATA_HOME or ~/.local/share, then dev.atlas.ide.
func AppDataDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "dev.atlas.ide")
	default:
		if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
			return filepath.Join(x, "dev.atlas.ide")
		}
		return filepath.Join(home, ".local", "share", "dev.atlas.ide")
	}
}

// ModelDir is where the model's files live; ATLAS_MODELS_DIR replaces the
// models directory.
func ModelDir() string {
	if d := os.Getenv("ATLAS_MODELS_DIR"); d != "" {
		return filepath.Join(d, ModelID)
	}
	return filepath.Join(AppDataDir(), "models", ModelID)
}

// Available reports that every model file is present.
func Available() bool {
	for _, f := range Files {
		if fi, err := os.Stat(filepath.Join(ModelDir(), f)); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// Embedder turns text into unit vectors.
type Embedder struct {
	tok   *Tokenizer
	model *Model
	mu    sync.Mutex
}

var (
	shared   *Embedder
	sharedMu sync.Mutex
)

// Default loads the model once per process; ErrNoModel while it is missing
// (checked again on each call, so a download in this process is picked up).
func Default() (*Embedder, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		return shared, nil
	}
	if !Available() {
		return nil, ErrNoModel
	}
	e, err := Load(ModelDir())
	if err != nil {
		return nil, err
	}
	shared = e
	return shared, nil
}

// Forget drops the loaded model (after its files were removed).
func Forget() {
	sharedMu.Lock()
	shared = nil
	sharedMu.Unlock()
}

// DiskSize is the bytes the model's files take, 0 when absent.
func DiskSize() int64 {
	var n int64
	for _, f := range Files {
		if fi, err := os.Stat(filepath.Join(ModelDir(), f)); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Remove deletes the model's files (the app uses the same ones).
func Remove() error {
	Forget()
	for _, f := range append(append([]string(nil), Files...), ".metal_incompatible") {
		if err := os.Remove(filepath.Join(ModelDir(), f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Load reads a model directory.
func Load(dir string) (*Embedder, error) {
	tok, err := LoadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	m, err := LoadModel(filepath.Join(dir, "config.json"), filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	return &Embedder{tok: tok, model: m}, nil
}

// Embed returns text's unit vector; an empty text is the zero vector.
func (e *Embedder) Embed(text string) []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := e.tok.Encode(text)
	if len(ids) <= 2 {
		return make([]float32, e.model.Dim())
	}
	return e.model.Forward(ids)
}

// CacheKey is blake3(model ‖ 0x00 ‖ text) (atlas-retrieval codec.rs).
func CacheKey(model, text string) []byte {
	h := blake3.New()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return h.Sum(nil)
}

// VKey is a document's vector key: the first 8 bytes of blake3(id),
// little-endian, as a non-negative int64 (corpus.rs doc_key).
func VKey(id string) int64 {
	sum := blake3.Sum256([]byte(id))
	return int64(binary.LittleEndian.Uint64(sum[:8]) & math.MaxInt64)
}

// Encode packs a vector as f16 little-endian, as the desktop stores it.
func Encode(v []float32) []byte {
	out := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(out[2*i:], f32to16(x))
	}
	return out
}

// Decode unpacks an f16 little-endian vector.
func Decode(b []byte) []float32 {
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = f16to32(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// Cosine of two vectors (unit vectors: their dot product).
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(na*nb))
}

// f32to16 rounds to nearest even, as half::f16::from_f32.
func f32to16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int((b >> 23) & 0xff)
	man := b & 0x7fffff
	switch {
	case exp == 0xff: // inf, NaN
		if man != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp > 142: // overflow
		return sign | 0x7c00
	case exp < 103: // underflow to zero
		return sign
	case exp < 113: // subnormal
		man |= 0x800000
		shift := uint(126 - exp)
		half := uint32(1) << (shift - 1)
		r := man >> shift
		rem := man & ((1 << shift) - 1)
		if rem > half || rem == half && r&1 == 1 {
			r++
		}
		return sign | uint16(r) //nolint:gosec // G115: r < 0x400 here
	}
	h := uint32(exp-112)<<10 | man>>13
	rem := man & 0x1fff
	if rem > 0x1000 || rem == 0x1000 && h&1 == 1 {
		h++
	}
	return sign | uint16(h) //nolint:gosec // G115: h ≤ 0x7c00 (exp ≤ 142)
}

func f16to32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	man := uint32(h & 0x3ff)
	switch exp {
	case 0:
		if man == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: normalise
		e := uint32(113)
		for man&0x400 == 0 {
			man <<= 1
			e--
		}
		man &= 0x3ff
		return math.Float32frombits(sign | e<<23 | man<<13)
	case 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | man<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | man<<13)
}
