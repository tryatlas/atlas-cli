package mcpconfig

// Antigravity CLI (agy): the workspace MCP config is .agents/mcp_config.json
// (the global one is ~/.gemini/config/mcp_config.json), a single mcpServers
// object whose stdio entries carry command, args, env and cwd
// (https://antigravity.google/docs/mcp?tab=cli). No "type" field: agy tells
// stdio from remote by command versus serverUrl.

const antigravityID = "antigravity"

func init() {
	Register(antigravityID, JSONServers{File: ".agents/mcp_config.json", Key: "mcpServers", AgentID: antigravityID})
}
