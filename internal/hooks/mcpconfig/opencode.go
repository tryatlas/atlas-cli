package mcpconfig

import (
	"encoding/json"

	"github.com/tryatlas/cli/internal/hooks"
)

// OpenCode: opencode.json at the Project root holds "mcp": {name: {type:
// "local", command: […], enabled}}. The command is one array (no separate
// args), so the entry is OpenCode-shaped rather than the stdio default.
// Every other key, $schema included, survives the edit.

const opencodeID = "opencode"

func init() {
	Register(opencodeID, JSONServers{File: "opencode.json", Key: "mcp", AgentID: opencodeID, Entry: opencodeEntry})
}

// opencodeEntry is the local MCP server entry OpenCode expects.
func opencodeEntry(root string) json.RawMessage {
	return hooks.Raw(map[string]any{
		"type":    "local",
		"command": append([]string{hooks.Bin}, Args(root, opencodeID)...),
		"enabled": true,
	})
}
