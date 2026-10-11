package mcpconfig

// Factory Droid: .factory/mcp.json at the Project root is the project-level
// MCP config, committed and shared with the team
// (docs.factory.com/cli/configuration/mcp). Servers live under mcpServers;
// a stdio entry is {"type":"stdio","command":…,"args":[…]} and Droid loads
// it without a per-server approval step. The user-level file ~/.factory/mcp.json
// is left alone.

const droidID = "droid"

func init() {
	Register(droidID, JSONServers{File: ".factory/mcp.json", Key: "mcpServers", AgentID: droidID, WithType: true})
}
