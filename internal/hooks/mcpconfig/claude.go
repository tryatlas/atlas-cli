package mcpconfig

import (
	"encoding/json"

	"github.com/tryatlas/cli/internal/hooks"
)

// Claude Code: .mcp.json at the Project root is the project-scope MCP config;
// Claude asks once before using a project server unless the local settings
// approve it by name (enabledMcpjsonServers), which Write also does.

const claudeID = "claude-code"

func init() { Register(claudeID, claudeWriter{}) }

type claudeWriter struct{}

var claudeServers = JSONServers{File: ".mcp.json", Key: "mcpServers", AgentID: claudeID, WithType: true}

func (claudeWriter) Path(root string) string { return claudeServers.Path(root) }

func (claudeWriter) Write(root string) (bool, error) {
	changed, err := claudeServers.Write(root)
	if err != nil {
		return false, err
	}
	approved, err := approveClaude(root)
	return changed || approved, err
}

func (claudeWriter) Remove(root string) error {
	if err := claudeServers.Remove(root); err != nil {
		return err
	}
	return unapproveClaude(root)
}

func (claudeWriter) State(root string) hooks.InstallState { return claudeServers.State(root) }

func claudeLocalSettings(root string) string { return root + "/.claude/settings.local.json" }

// approveClaude lists atlas_memory in enabledMcpjsonServers of the local
// settings so Claude does not prompt for it.
func approveClaude(root string) (bool, error) {
	obj, err := hooks.LoadJSONObject(claudeLocalSettings(root))
	if err != nil {
		return false, err
	}
	var list []string
	if raw, ok := obj["enabledMcpjsonServers"]; ok {
		_ = json.Unmarshal(raw, &list)
	}
	for _, s := range list {
		if s == ServerName {
			return false, nil
		}
	}
	obj["enabledMcpjsonServers"] = hooks.Raw(append(list, ServerName))
	return true, hooks.SaveJSON(claudeLocalSettings(root), obj, 0o600)
}

func unapproveClaude(root string) error {
	obj, err := hooks.LoadJSONObject(claudeLocalSettings(root))
	if err != nil {
		return err
	}
	raw, ok := obj["enabledMcpjsonServers"]
	if !ok {
		return nil
	}
	var list, keep []string
	_ = json.Unmarshal(raw, &list)
	for _, s := range list {
		if s != ServerName {
			keep = append(keep, s)
		}
	}
	if len(keep) == len(list) {
		return nil
	}
	if len(keep) == 0 {
		delete(obj, "enabledMcpjsonServers")
	} else {
		obj["enabledMcpjsonServers"] = hooks.Raw(keep)
	}
	return hooks.SaveJSON(claudeLocalSettings(root), obj, 0o600)
}
