package mcpconfig

// Pi: .pi/mcp.json at the Project root holds the project MCP servers under
// mcpServers; a stdio server is {"command","args"} (type is optional and
// implied by command). Pi reads the project file only once the project is
// trusted, and a project entry replaces a user entry of the same name
// (pi-mono packages/coding-agent/docs/mcp.md).

const piID = "pi"

func init() {
	Register(piID, JSONServers{File: ".pi/mcp.json", Key: "mcpServers", AgentID: piID})
}
