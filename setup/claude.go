package setup

import (
	"context"
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
			if err := claudeMCPAdd(exe, run); err != nil {
				return err
			}
			return applyHooks(settings, exe, claudeAgent)
		},
		Remove: func() error {
			if err := claudeMCPRemove(run); err != nil {
				return err
			}
			return removeHooks(settings, exe, claudeAgent)
		},
		Check: func() (bool, error) {
			out, err := run(context.Background(), "claude", "mcp", "get", "agentnet")
			if err != nil {
				return false, nil
			}
			return strings.Contains(string(out), exe), nil
		},
	}
}

// claudeMCPAdd registers agentnet's MCP server with Claude Code unless it's already pointed at exe.
func claudeMCPAdd(exe string, run Runner) error {
	out, err := run(context.Background(), "claude", "mcp", "get", "agentnet")
	if err == nil && strings.Contains(string(out), exe) {
		return nil
	}
	_, err = run(context.Background(), "claude", "mcp", "add", "--scope", "user", "agentnet", "--", exe, "mcp")
	return err
}

// claudeMCPRemove unregisters agentnet's MCP server from Claude Code.
func claudeMCPRemove(run Runner) error {
	_, err := run(context.Background(), "claude", "mcp", "remove", "--scope", "user", "agentnet")
	return err
}
