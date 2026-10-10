package ambient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Node is an order-preserving JSON value, so a user's config file keeps its
// key order when the CLI adds or removes its own entries.
type Node struct {
	Kind   byte // 'o' object, 'a' array, 'v' any other value (raw)
	Keys   []string
	Fields map[string]*Node
	Items  []*Node
	Raw    json.RawMessage
}

// Obj is an empty object.
func Obj() *Node { return &Node{Kind: 'o', Fields: map[string]*Node{}} }

// Arr is an empty array.
func Arr() *Node { return &Node{Kind: 'a'} }

// Val wraps a Go value.
func Val(v any) *Node {
	b, _ := marshal(v)
	return &Node{Kind: 'v', Raw: b}
}

// Get returns a field, or nil.
func (n *Node) Get(k string) *Node {
	if n == nil || n.Kind != 'o' {
		return nil
	}
	return n.Fields[k]
}

// Set adds or replaces a field, keeping its position when present.
func (n *Node) Set(k string, v *Node) {
	if _, ok := n.Fields[k]; !ok {
		n.Keys = append(n.Keys, k)
	}
	n.Fields[k] = v
}

// Delete removes a field.
func (n *Node) Delete(k string) {
	if _, ok := n.Fields[k]; !ok {
		return
	}
	delete(n.Fields, k)
	for i, x := range n.Keys {
		if x == k {
			n.Keys = append(n.Keys[:i], n.Keys[i+1:]...)
			break
		}
	}
}

// Child returns the object at k, creating it when absent.
func (n *Node) Child(k string) *Node {
	if c := n.Get(k); c != nil && c.Kind == 'o' {
		return c
	}
	c := Obj()
	n.Set(k, c)
	return c
}

// String reads a string value.
func (n *Node) String() (string, bool) {
	if n == nil || n.Kind != 'v' {
		return "", false
	}
	var s string
	return s, json.Unmarshal(n.Raw, &s) == nil
}

// Parse reads JSON keeping key order.
func Parse(b []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := parse(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return n, nil
}

func parse(dec *json.Decoder) (*Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := Obj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := parse(dec)
				if err != nil {
					return nil, err
				}
				n.Set(k, v)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := Arr()
			for dec.More() {
				v, err := parse(dec)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, v)
			}
			_, err := dec.Token()
			return n, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		b, err := marshal(t)
		return &Node{Kind: 'v', Raw: b}, err
	}
}

// Marshal writes compact JSON in key order.
func (n *Node) Marshal() []byte {
	var b bytes.Buffer
	n.write(&b)
	return b.Bytes()
}

func (n *Node) write(b *bytes.Buffer) {
	switch n.Kind {
	case 'o':
		b.WriteByte('{')
		for i, k := range n.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			n.Fields[k].write(b)
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			it.write(b)
		}
		b.WriteByte(']')
	default:
		b.Write(n.Raw)
	}
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// LoadFile reads a JSON object file; a missing file is an empty object.
func LoadFile(path string) (*Node, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: an agent config file in the Project
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(b)) == 0) {
		return Obj(), nil
	}
	if err != nil {
		return nil, err
	}
	n, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON; fix or remove it first: %w", path, err)
	}
	if n.Kind != 'o' {
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	return n, nil
}

// SaveFile writes the object pretty-printed (two spaces, trailing newline)
// through a temp file and rename. An empty object removes the file when the
// CLI created it (removeEmpty).
func SaveFile(path string, n *Node, removeEmpty bool) error {
	if removeEmpty && n.Kind == 'o' && len(n.Keys) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, n.Marshal(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".atlas-tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil { //nolint:gosec // G306: agent config, no secret (ADR-0003)
		return err
	}
	return os.Rename(tmp, path)
}

// quote makes a command-line word safe for sh.
func quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`&|;<>()*?[]{}~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
