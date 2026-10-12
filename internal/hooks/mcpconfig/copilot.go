package mcpconfig

import (
	"encoding/json"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// GitHub Copilot CLI reads MCP servers from three tiers (docs.github.com,
// Copilot CLI command reference, "MCP server trust levels"): the user's
// ~/.copilot/mcp-config.json, the repository's .github/mcp.json, and the
// workspace's .mcp.json, all shaped {"mcpServers":{name:entry}}. Atlas
// writes the repository tier: it is Copilot's own file (unlike .mcp.json,
// which the Claude Code writer already owns and whose entry carries a
// different --agent), and it stays with the Project rather than binding the
// whole machine to one Project as the user file would. A stdio server is
// Copilot's "local" type; tools lists what the model may call. Like the
// repository's hooks, .github/mcp.json is read only from a trusted working
// directory (interactive trust prompt, COPILOT_ALLOW_ALL=true, or
// GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP=true for copilot -p); the Copilot
// agent's AddNote tells the user.

const copilotID = "copilot"

func init() { Register(copilotID, copilotServers) }

var copilotServers = JSONServers{File: ".github/mcp.json", Key: "mcpServers", AgentID: copilotID, Entry: copilotEntry}

// copilotEntry is {"type":"local","command":"atlas-cli","args":[…],"tools":["*"]}.
func copilotEntry(root string) json.RawMessage {
	return hooks.Raw(map[string]any{
		"type":    "local",
		"command": hooks.Bin,
		"args":    Args(root, copilotID),
		"tools":   []string{"*"},
	})
}
