package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// traceExchange writes the shape of an exchange (never its text or headers)
// to the file named by ATLAS_PROXY_TRACE, for diagnosing what an agent sends.
func traceExchange(ex Exchange) {
	path := os.Getenv("ATLAS_PROXY_TRACE")
	if path == "" {
		return
	}
	var shape []string
	if req, ok := ParseRequest(ex.Request); ok {
		for _, m := range req.Messages {
			var kinds []string
			for _, b := range m.Blocks() {
				k := b.Type
				if b.Type == "text" {
					k = fmt.Sprintf("text(%d,reminder=%v)", len(b.Text), strings.Contains(b.Text, "<system-reminder>"))
				}
				kinds = append(kinds, k)
			}
			shape = append(shape, m.Role+"["+strings.Join(kinds, ",")+"]")
		}
		shape = append([]string{fmt.Sprintf("model=%s tools=%d user_id_has_session=%v", req.Model, len(req.Tools),
			strings.Contains(req.Metadata.UserID, "session"))}, shape...)
	} else {
		shape = []string{fmt.Sprintf("unparsed request (%d bytes)", len(ex.Request))}
	}
	resp := "unparsed"
	if r, ok := ParseResponse(ex.ContentType, ex.Response); ok {
		var bt []string
		for _, b := range r.Blocks {
			bt = append(bt, b.Type)
		}
		resp = fmt.Sprintf("id=%t stop=%s blocks=%v usage=%+v", r.ID != "", r.StopReason, bt, r.Usage)
	}
	line, _ := json.Marshal(map[string]any{"path": ex.Path, "status": ex.Status, "ctype": ex.ContentType,
		"enc_ok": ex.Response != nil, "request": shape, "response": resp})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path the user chose
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
