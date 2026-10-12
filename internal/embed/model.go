package embed

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
)

// Config is the part of config.json the forward pass needs.
type Config struct {
	Hidden       int     `json:"hidden_size"`
	Layers       int     `json:"num_hidden_layers"`
	Heads        int     `json:"num_attention_heads"`
	Intermediate int     `json:"intermediate_size"`
	MaxPositions int     `json:"max_position_embeddings"`
	Vocab        int     `json:"vocab_size"`
	Eps          float64 `json:"layer_norm_eps"`
	Act          string  `json:"hidden_act"`
}

type linear struct {
	w   []float32 // out × in, row-major (PyTorch's layout)
	b   []float32
	in  int
	out int
}

type layerNorm struct{ w, b []float32 }

type layer struct {
	q, k, v, o    linear
	attnNorm      layerNorm
	inter, output linear
	outNorm       layerNorm
}

// Model is a BERT encoder in float32 (as candle runs it on the CPU).
type Model struct {
	cfg            Config
	word, pos, typ []float32
	embNorm        layerNorm
	layers         []layer
}

// tensors reads a safetensors file: an 8-byte header length, a JSON header
// of name → {dtype, shape, data_offsets}, then the data.
func tensors(path string) (map[string][]float32, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the model's own file
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, fmt.Errorf("%s: too short", path)
	}
	n := binary.LittleEndian.Uint64(b[:8])
	if n > uint64(len(b)-8) { //nolint:gosec // G115: len(b) ≥ 8 here
		return nil, fmt.Errorf("%s: bad header", path)
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(b[8:8+n], &hdr); err != nil {
		return nil, err
	}
	data := b[8+n:]
	out := map[string][]float32{}
	for name, raw := range hdr {
		if name == "__metadata__" {
			continue
		}
		var t struct {
			Dtype   string   `json:"dtype"`
			Offsets [2]int64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		if t.Dtype != "F32" {
			continue // position_ids (I64) and the like
		}
		lo, hi := t.Offsets[0], t.Offsets[1]
		if lo < 0 || hi > int64(len(data)) || lo > hi || (hi-lo)%4 != 0 {
			return nil, fmt.Errorf("%s: bad offsets for %s", path, name)
		}
		f := make([]float32, (hi-lo)/4)
		for i := range f {
			f[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[lo+int64(i)*4:]))
		}
		out[name] = f
	}
	return out, nil
}

// LoadModel reads config.json and model.safetensors.
func LoadModel(configPath, weightsPath string) (*Model, error) {
	cb, err := os.ReadFile(configPath) //nolint:gosec // G304: the model's own file
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return nil, err
	}
	if cfg.Eps == 0 {
		cfg.Eps = 1e-12
	}
	if cfg.Hidden == 0 || cfg.Heads == 0 || cfg.Hidden%cfg.Heads != 0 {
		return nil, fmt.Errorf("config.json: bad sizes")
	}
	ts, err := tensors(weightsPath)
	if err != nil {
		return nil, err
	}
	var missing string
	get := func(name string, size int) []float32 {
		for _, n := range []string{name, "bert." + name} {
			if t, ok := ts[n]; ok && len(t) == size {
				return t
			}
		}
		if missing == "" {
			missing = name
		}
		return make([]float32, size)
	}
	h, in := cfg.Hidden, cfg.Intermediate
	lin := func(p string, inDim, outDim int) linear {
		return linear{w: get(p+".weight", inDim*outDim), b: get(p+".bias", outDim), in: inDim, out: outDim}
	}
	ln := func(p string) layerNorm { return layerNorm{w: get(p+".weight", h), b: get(p+".bias", h)} }
	m := &Model{cfg: cfg,
		word:    get("embeddings.word_embeddings.weight", cfg.Vocab*h),
		pos:     get("embeddings.position_embeddings.weight", cfg.MaxPositions*h),
		typ:     get("embeddings.token_type_embeddings.weight", 2*h),
		embNorm: ln("embeddings.LayerNorm"),
	}
	for i := range cfg.Layers {
		p := fmt.Sprintf("encoder.layer.%d.", i)
		m.layers = append(m.layers, layer{
			q: lin(p+"attention.self.query", h, h), k: lin(p+"attention.self.key", h, h), v: lin(p+"attention.self.value", h, h),
			o: lin(p+"attention.output.dense", h, h), attnNorm: ln(p + "attention.output.LayerNorm"),
			inter: lin(p+"intermediate.dense", h, in), output: lin(p+"output.dense", in, h), outNorm: ln(p + "output.LayerNorm"),
		})
	}
	if missing != "" {
		return nil, fmt.Errorf("model.safetensors: no tensor %s", missing)
	}
	return m, nil
}

// Dim is the embedding size.
func (m *Model) Dim() int { return m.cfg.Hidden }

// apply computes y = x·Wᵀ + b for n rows, the rows split across cores.
func (l linear) apply(x []float32, n int) []float32 {
	y := make([]float32, n*l.out)
	workers := min(runtime.GOMAXPROCS(0), max(l.out/32, 1))
	var wg sync.WaitGroup
	chunk := (l.out + workers - 1) / workers
	for w := range workers {
		lo, hi := w*chunk, min((w+1)*chunk, l.out)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := lo; o < hi; o++ {
				wr := l.w[o*l.in : (o+1)*l.in]
				bias := l.b[o]
				for r := range n {
					y[r*l.out+o] = dot(x[r*l.in:(r+1)*l.in], wr) + bias
				}
			}
		}()
	}
	wg.Wait()
	return y
}

func dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

func (ln layerNorm) apply(x []float32, n, h int, eps float64) {
	for r := range n {
		row := x[r*h : (r+1)*h]
		var mean float64
		for _, v := range row {
			mean += float64(v)
		}
		mean /= float64(h)
		var vr float64
		for _, v := range row {
			d := float64(v) - mean
			vr += d * d
		}
		inv := 1 / math.Sqrt(vr/float64(h)+eps)
		for i, v := range row {
			row[i] = float32((float64(v)-mean)*inv)*ln.w[i] + ln.b[i]
		}
	}
}

func gelu(x float32) float32 {
	return float32(0.5 * float64(x) * (1 + math.Erf(float64(x)/math.Sqrt2)))
}

// Forward runs the encoder on one sequence and returns the mean of its
// token states (every token is real: one text, no padding), L2-normalised.
func (m *Model) Forward(ids []int32) []float32 {
	h, n := m.cfg.Hidden, len(ids)
	if n == 0 {
		return make([]float32, h)
	}
	x := make([]float32, n*h)
	for t, id := range ids {
		if int(id) >= m.cfg.Vocab || t >= m.cfg.MaxPositions {
			continue
		}
		row := x[t*h : (t+1)*h]
		for i := range h {
			row[i] = m.word[int(id)*h+i] + m.pos[t*h+i] + m.typ[i]
		}
	}
	eps := m.cfg.Eps
	m.embNorm.apply(x, n, h, eps)
	heads := m.cfg.Heads
	hd := h / heads
	scale := float32(1 / math.Sqrt(float64(hd)))
	for _, L := range m.layers {
		q, k, v := L.q.apply(x, n), L.k.apply(x, n), L.v.apply(x, n)
		ctx := make([]float32, n*h)
		var wg sync.WaitGroup
		for hh := range heads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scores := make([]float32, n)
				for i := range n {
					qi := q[i*h+hh*hd : i*h+hh*hd+hd]
					maxS := float32(math.Inf(-1))
					for j := range n {
						s := dot(qi, k[j*h+hh*hd:j*h+hh*hd+hd]) * scale
						scores[j] = s
						maxS = max(maxS, s)
					}
					var sum float32
					for j := range n {
						scores[j] = float32(math.Exp(float64(scores[j] - maxS)))
						sum += scores[j]
					}
					out := ctx[i*h+hh*hd : i*h+hh*hd+hd]
					for j := range n {
						p := scores[j] / sum
						vj := v[j*h+hh*hd : j*h+hh*hd+hd]
						for d := range hd {
							out[d] += p * vj[d]
						}
					}
				}
			}()
		}
		wg.Wait()
		a := L.o.apply(ctx, n)
		for i := range a {
			a[i] += x[i]
		}
		L.attnNorm.apply(a, n, h, eps)
		inter := L.inter.apply(a, n)
		for i := range inter {
			inter[i] = gelu(inter[i])
		}
		out := L.output.apply(inter, n)
		for i := range out {
			out[i] += a[i]
		}
		L.outNorm.apply(out, n, h, eps)
		x = out
	}
	pooled := make([]float32, h)
	for t := range n {
		for i := range h {
			pooled[i] += x[t*h+i]
		}
	}
	var norm float64
	for i := range pooled {
		pooled[i] /= float32(n)
		norm += float64(pooled[i]) * float64(pooled[i])
	}
	norm = math.Max(math.Sqrt(norm), 1e-12)
	for i := range pooled {
		pooled[i] = float32(float64(pooled[i]) / norm)
	}
	return pooled
}
