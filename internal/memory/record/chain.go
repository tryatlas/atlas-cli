package record

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/zeebo/blake3"
)

// chainColumns are the revision columns sealed into the chain, in order
// (SPEC §5.4, record.rs CHAIN_COLUMNS). note is outside the chain.
const chainColumns = `rev, entry_id, op, kind, key, content, content_hash, status, source, agent, session, confidence, state, scope, evidence, at`

// Value type tags of the chain encoding.
const (
	tagNull byte = 0
	tagInt  byte = 1
	tagReal byte = 2
	tagText byte = 3
	tagBlob byte = 4
)

// rowBytes encodes one revision's chain columns: per value a 1-byte tag, a
// u64 little-endian length, then the bytes (i64 LE, f64 bits LE, UTF-8, raw).
func rowBytes(values []any) ([]byte, error) {
	var out []byte
	var n [8]byte
	put := func(tag byte, b []byte) {
		out = append(out, tag)
		binary.LittleEndian.PutUint64(n[:], uint64(len(b)))
		out = append(out, n[:]...)
		out = append(out, b...)
	}
	for _, v := range values {
		switch x := v.(type) {
		case nil:
			put(tagNull, nil)
		case int64:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], uint64(x)) //nolint:gosec // G115: two's-complement bits, as i64::to_le_bytes
			put(tagInt, b[:])
		case float64:
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(x))
			put(tagReal, b[:])
		case string:
			put(tagText, []byte(x))
		case []byte:
			put(tagBlob, x)
		default:
			return nil, fmt.Errorf("chain: unsupported value %T", v)
		}
	}
	return out, nil
}

// link is blake3(prev ‖ row).
func link(prev, row []byte) []byte {
	h := blake3.New()
	_, _ = h.Write(prev)
	_, _ = h.Write(row)
	return h.Sum(nil)
}

// genesis is the previous link before the first revision.
var genesis = make([]byte, 32)

// ChainReport is the outcome of VerifyChain.
type ChainReport struct {
	Revisions int   // revisions checked
	FirstBad  int64 // rev of the first revision whose link does not match; 0 when intact
}

// OK reports an intact chain.
func (r ChainReport) OK() bool { return r.FirstBad == 0 }

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// verifyChain recomputes every link in rev order.
func verifyChain(db querier) (ChainReport, error) {
	rows, err := db.Query(`SELECT ` + chainColumns + `, chain FROM revisions ORDER BY rev`)
	if err != nil {
		return ChainReport{}, err
	}
	defer rows.Close()
	var rep ChainReport
	prev := genesis
	for rows.Next() {
		vals := make([]any, 17)
		ptrs := make([]any, 17)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return rep, err
		}
		stored, _ := vals[16].([]byte)
		row, err := rowBytes(normalise(vals[:16]))
		if err != nil {
			return rep, err
		}
		want := link(prev, row)
		rep.Revisions++
		if rep.FirstBad == 0 && string(want) != string(stored) {
			rep.FirstBad, _ = vals[0].(int64)
		}
		prev = stored // each link seals over the stored previous link
	}
	return rep, rows.Err()
}

// normalise maps driver values to the encoding's types.
func normalise(vals []any) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		switch x := v.(type) {
		case int:
			out[i] = int64(x)
		case int32:
			out[i] = int64(x)
		case float32:
			out[i] = float64(x)
		default:
			out[i] = v
		}
	}
	return out
}
