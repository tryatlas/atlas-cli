package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// reader reads agy's step JSONL transcript
// (~/.gemini/antigravity-cli/brain/<conversation>/.system_generated/logs/transcript_full.jsonl):
// one step object per line with step_index, source (USER_EXPLICIT | MODEL |
// SYSTEM), type (USER_INPUT | PLANNER_RESPONSE | CONVERSATION_HISTORY |
// RUN_COMMAND | GENERIC | …), content, thinking, tool_calls,
// truncated_fields and, on PLANNER_RESPONSE steps since agy 1.3,
// input_tokens, output_tokens and cache_read_tokens. Positions count
// non-blank lines, so a blank line agy may leave never shifts an offset.
type reader struct{}

// Position is the non-blank line count up to and including the last step
// the model wrote; a transcript not yet written is at 0. The steps after it
// (the USER_INPUT of the turn about to start, agy's CONVERSATION_HISTORY
// bookkeeping) are left for the next Read: agy writes the prompt step
// before the PreInvocation hook fires, so a position at the end of the file
// would lose it.
func (reader) Position(path string) (int, error) {
	n := 0
	err := eachNonBlankLine(path, func(i int, raw []byte) {
		var s struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(raw, &s) == nil && s.Source == "MODEL" {
			n = i + 1
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return n, err
}

// Settle waits for agy's late transcript: the file is written after the
// Stop payload is sent, sometimes seconds later. It waits for the file to
// appear and then for its size to hold still.
func (reader) Settle(ctx context.Context, path string) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	hooks.SettleFile(ctx, path, 500*time.Millisecond, 3*time.Second)
}

// step is one line of the transcript. The token fields are pointers so a
// step that carries none (agy before 1.3, non-model steps) is told from a
// real zero.
type step struct {
	StepIndex       int             `json:"step_index"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Content         string          `json:"content"`
	Thinking        string          `json:"thinking"`
	ToolCalls       []toolCall      `json:"tool_calls"`
	TruncatedFields json.RawMessage `json:"truncated_fields"`
	InputTokens     *int64          `json:"input_tokens"`
	OutputTokens    *int64          `json:"output_tokens"`
	CacheReadTokens *int64          `json:"cache_read_tokens"`
	ThinkingTokens  *int64          `json:"thinking_tokens"`
}

// usage adds a step's token counts to t when it carries any.
func (s *step) usage(t *hooks.Turn) {
	if s.InputTokens == nil && s.OutputTokens == nil && s.CacheReadTokens == nil && s.ThinkingTokens == nil {
		return
	}
	t.HasUsage = true
	add := func(dst *int64, v *int64) {
		if v != nil {
			*dst += *v
		}
	}
	add(&t.Usage.Input, s.InputTokens)
	add(&t.Usage.Output, s.OutputTokens)
	add(&t.Usage.CacheRead, s.CacheReadTokens)
	add(&t.Usage.Reasoning, s.ThinkingTokens)
}

type toolCall struct {
	Name string                     `json:"name"`
	Args map[string]json.RawMessage `json:"args"`
}

// userRequest is the typed prompt inside a USER_INPUT step; the rest of the
// content is agy's own metadata.
var userRequest = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// prompt unwraps a USER_INPUT step's content; content without the wrapper
// is taken as the prompt itself.
func prompt(content string) string {
	if m := userRequest.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(content)
}

// Read parses the steps from a position into a turn. A step agy marks as
// truncated is still read for what it holds. Usage is the sum of the steps'
// token fields when any step has them. No model: the transcript names none
// (the hook payload does).
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	t := &hooks.Turn{}
	var open []int // indexes of tool items awaiting an output step
	pos := from
	err := eachNonBlankLine(path, func(i int, raw []byte) {
		pos = i + 1
		if i < from {
			return
		}
		var s step
		if json.Unmarshal(raw, &s) != nil {
			return
		}
		switch s.Type {
		case "USER_INPUT":
			if p := prompt(s.Content); p != "" {
				t.Prompts = append(t.Prompts, p)
			}
		case "PLANNER_RESPONSE":
			s.usage(t)
			id := strconv.Itoa(s.StepIndex)
			if body := strings.TrimSpace(s.Thinking); body != "" {
				t.Items = append(t.Items, hooks.Item{ID: id + "-thinking", Mode: "thinking", Body: s.Thinking})
			}
			if body := strings.TrimSpace(s.Content); body != "" {
				t.Items = append(t.Items, hooks.Item{ID: id + "-text", Mode: "text", Body: s.Content})
			}
			for i, tc := range s.ToolCalls {
				if tc.Name == "" {
					continue
				}
				tool := &record.Tool{
					NativeID:  id + "-" + strconv.Itoa(i),
					Name:      tc.Name,
					Kind:      toolKinds[tc.Name],
					Arguments: decodeArgs(tc.Args),
					Status:    "completed",
				}
				if writeTools[tc.Name] {
					if target, _ := decodeString(tc.Args["TargetFile"]); target != "" {
						tool.Locations = []string{target}
					}
				}
				open = append(open, len(t.Items))
				t.Items = append(t.Items, hooks.Item{Tool: tool})
			}
		case "CONVERSATION_HISTORY", "SYSTEM_MESSAGE":
			// agy's own bookkeeping and injected notices.
		default:
			// A MODEL step of any other type is a tool's output, in the
			// order the calls were made.
			if s.Source != "MODEL" || len(open) == 0 {
				return
			}
			tool := t.Items[open[0]].Tool
			open = open[1:]
			out := s.Content
			tool.Result = &out
			if s.Status == "ERROR" {
				tool.Status = "failed"
			}
		}
	})
	if err != nil {
		return nil, from, err
	}
	if pos < from {
		pos = from
	}
	return t, pos, nil
}

// eachNonBlankLine calls fn with each non-blank line and its 0-based
// non-blank index. Lines may be arbitrarily long.
func eachNonBlankLine(path string, fn func(i int, line []byte)) error {
	f, err := os.Open(path) //nolint:gosec // G304: the transcript the agent's payload named
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	n := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			fn(n, bytes.TrimRight(line, "\r\n"))
			n++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
