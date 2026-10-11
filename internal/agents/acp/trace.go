package acp

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// ATLAS_ACP_TRACE names a file that receives the shape of every message on
// the wire: direction, method, update kind, tool call id and status, and the
// keys present. Never a value: no prompt, file content or token is written.

var traceMu sync.Mutex

func trace(dir string, line []byte) {
	path := os.Getenv("ATLAS_ACP_TRACE")
	if path == "" {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return
	}
	rec := map[string]any{"t": time.Now().UTC().Format("15:04:05.000"), "dir": dir, "keys": keysOf(m)}
	var method string
	_ = json.Unmarshal(m["method"], &method)
	if method != "" {
		rec["method"] = method
	}
	if _, ok := m["id"]; ok {
		rec["id"] = string(m["id"])
	}
	if e, ok := m["error"]; ok {
		var er struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(e, &er)
		rec["error"] = er.Code
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(m["params"], &p)
	if len(p) > 0 {
		rec["params"] = keysOf(p)
	}
	var u map[string]json.RawMessage
	_ = json.Unmarshal(p["update"], &u)
	if len(u) > 0 {
		var kind, id, status string
		_ = json.Unmarshal(u["sessionUpdate"], &kind)
		_ = json.Unmarshal(u["toolCallId"], &id)
		_ = json.Unmarshal(u["status"], &status)
		rec["update"] = map[string]any{"kind": kind, "toolCallId": id, "status": status, "keys": keysOf(u)}
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(m["result"], &r) == nil && len(r) > 0 {
		rec["result"] = keysOf(r)
	}
	b, _ := json.Marshal(rec)
	traceMu.Lock()
	defer traceMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path the user chose
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
