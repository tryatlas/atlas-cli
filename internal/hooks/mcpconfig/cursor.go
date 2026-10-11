package mcpconfig

// Cursor: .cursor/mcp.json is the project MCP config, {"mcpServers":{name:
// {"command","args"}}}; Cursor asks once before enabling a project server.

const cursorID = "cursor"

func init() {
	Register(cursorID, JSONServers{File: ".cursor/mcp.json", Key: "mcpServers", AgentID: cursorID})
}
