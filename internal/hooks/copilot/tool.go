package copilot

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// toolPayload is Copilot's postToolUse input (1.0.95): {sessionId,
// timestamp, cwd, toolName, toolArgs, toolResult}. toolArgs is the tool's
// arguments object ({"command":…,"description":…} for bash, {} for an MCP
// tool), or apply_patch's bare patch string. toolResult is
// {"resultType":"success"|…,"textResultForLlm":…}; a shell command that
// exited non-zero is still a "success" (the tool ran), as in the transcript.
// The payload carries no tool call id: the transcript's toolCallId is not in
// it.
type toolPayload struct {
	SessionID  string          `json:"sessionId"`
	Timestamp  json.RawMessage `json:"timestamp"`
	ToolName   string          `json:"toolName"`
	ToolArgs   json.RawMessage `json:"toolArgs"`
	ToolResult json.RawMessage `json:"toolResult"`
}

type toolResult struct {
	ResultType string `json:"resultType"`
	Text       string `json:"textResultForLlm"`
}

// toolOf is the completed call a postToolUse payload describes, or nil when
// it names no tool. cwd resolves the relative paths of a patch.
func toolOf(raw []byte, cwd string) *record.Tool {
	var p toolPayload
	if json.Unmarshal(raw, &p) != nil || p.ToolName == "" {
		return nil
	}
	args := toolArguments(p.ToolArgs)
	t := &record.Tool{
		NativeID:  hookCallID(p.SessionID, string(bytes.TrimSpace(p.Timestamp)), p.ToolName, args),
		Name:      p.ToolName,
		Arguments: args,
		Locations: patchPaths(p.ToolArgs, cwd),
		Status:    "completed",
	}
	res := bytes.TrimSpace(p.ToolResult)
	if !isSet(res) {
		return t
	}
	var r toolResult
	if res[0] == '{' && json.Unmarshal(res, &r) == nil {
		if r.ResultType != "" && r.ResultType != "success" {
			t.Status = "failed"
		}
		text := r.Text
		if text == "" {
			text = string(res)
		}
		t.Result = &text
	} else if text, ok := resultText(res); ok {
		t.Result = &text
	}
	return t
}

// hookCallID stands in for the call id the payload lacks: a digest of the
// session, the payload's timestamp, the tool and its arguments, so a
// re-delivered payload upserts the same row and two calls of one tool in the
// same millisecond stay apart. It never equals the transcript's toolCallId,
// so the dispatcher's dedupe at turn end goes by the arguments hash, the
// shell command text and the written paths, all of which this package shapes
// the same way on both sides.
func hookCallID(session, timestamp, name string, args json.RawMessage) string {
	var compact bytes.Buffer
	_ = json.Compact(&compact, args)
	sum := store.SHA256Hex([]byte(session + "\x00" + timestamp + "\x00" + name + "\x00" + compact.String()))
	return "hook-" + sum[:24]
}

// toolArguments normalises a call's arguments to the JSON object the
// recorder and the dispatcher's dedupe keys read. An object stays as it is.
// apply_patch's bare patch string becomes {"patch": …}, which
// record.EditPatch keeps as the edit's patch; any other string becomes
// {"input": …}. Nothing stays nothing. The transcript reader applies the
// same shape, so a hook-delivered call and its transcript item hash alike.
func toolArguments(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if !isSet(raw) {
		return nil
	}
	if raw[0] != '"' {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return raw
	}
	key := "input"
	if isPatch(s) {
		key = "patch"
	}
	b, _ := json.Marshal(map[string]string{key: s})
	return b
}

// isPatch recognises the apply_patch format.
func isPatch(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "*** Begin Patch") }

// patchHeaders introduce the files a patch touches.
var patchHeaders = []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}

// patchPaths are the files an apply_patch call names, made absolute against
// cwd so they equal the transcript's fileEdits paths, which the dispatcher
// dedupes written paths by. Non-patch arguments yield nil: record.WritePaths
// reads their path keys instead.
func patchPaths(raw json.RawMessage, cwd string) []string {
	var s string
	if json.Unmarshal(raw, &s) != nil || !isPatch(s) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		var p string
		for _, h := range patchHeaders {
			if rest, ok := strings.CutPrefix(line, h); ok {
				p = strings.TrimSpace(rest)
				break
			}
		}
		if p == "" {
			continue
		}
		if cwd != "" && !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
