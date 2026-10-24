package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// claudeAgent is the agent name used in Claude Code's hook commands and for identifying our own hooks.
const claudeAgent = "claude-code"

// claudeStep registers agentnet's MCP server with Claude Code and wires its five lifecycle hooks.
func claudeStep(home, exe string, run Runner) Step {
	settings := filepath.Join(home, ".claude", "settings.json")
	return Step{
		Name: "Claude Code: MCP server and hooks",
		Apply: func() error {
			errMCP := claudeMCPApply(exe, run)
			errHooks := applyHooks(settings, exe, claudeAgent)
			return errors.Join(errMCP, errHooks)
		},
		Remove: func() error {
			errMCP := claudeMCPRemove(run)
			errHooks := removeHooks(settings, exe, claudeAgent)
			return errors.Join(errMCP, errHooks)
		},
		Check: func() (bool, error) {
			return claudeMCPApplied(home, exe)
		},
	}
}

// claudeMCPApply (re-)registers agentnet's MCP server with Claude Code. Claude Code's CLI has no
// "update" verb, so this removes any existing registration (ignoring "not registered") and adds
// a fresh one pointed at exe — idempotent, and unlike `claude mcp get`, it never spawns the server.
func claudeMCPApply(exe string, run Runner) error {
	if err := claudeMCPRemove(run); err != nil {
		return err
	}
	_, err := run(context.Background(), "claude", "mcp", "add", "--scope", "user", "agentnet", "--", exe, "mcp")
	return err
}

// claudeNotFound is the substring Claude Code's CLI prints when "agentnet" isn't registered.
const claudeNotFound = "No MCP server named"

// claudeMCPRemove unregisters agentnet's MCP server from Claude Code; already-absent is not an error.
func claudeMCPRemove(run Runner) error {
	out, err := run(context.Background(), "claude", "mcp", "remove", "--scope", "user", "agentnet")
	if err != nil && !strings.Contains(string(out), claudeNotFound) {
		return err
	}
	return nil
}

// claudeMCPApplied reads <home>/.claude.json directly: never invokes the claude binary, since
// `claude mcp get` actually spawns the configured server as a health check.
func claudeMCPApplied(home, exe string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false, nil
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, nil
	}
	return doc.MCPServers["agentnet"].Command == exe, nil
}
