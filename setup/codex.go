package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// codexAgent is the agent name used in Codex's hook commands and for identifying our own hooks.
const codexAgent = "codex"

// codexStep registers agentnet's MCP server in Codex's config.toml and wires its five lifecycle hooks.
func codexStep(home, exe string, run Runner) Step {
	configPath := filepath.Join(home, ".codex", "config.toml")
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	return Step{
		Name: "Codex: MCP server and hooks (trust new hooks once in /hooks)",
		Apply: func() error {
			if err := applyCodexConfig(configPath, exe); err != nil {
				return err
			}
			return applyHooks(hooksPath, exe, codexAgent)
		},
		Remove: func() error {
			if err := removeCodexConfig(configPath); err != nil {
				return err
			}
			return removeHooks(hooksPath, exe, codexAgent)
		},
		Check: func() (bool, error) {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return false, nil
			}
			return strings.Contains(string(data), "[mcp_servers.agentnet]") && strings.Contains(string(data), exe), nil
		},
	}
}

// codexBlock is the exact lines setup owns inside config.toml.
func codexBlock(exe string) []string {
	return []string{
		"[mcp_servers.agentnet]",
		fmt.Sprintf(`command = "%s"`, exe),
		`args = ["mcp"]`,
	}
}

// applyCodexConfig appends, or in place replaces, our [mcp_servers.agentnet] table in path.
func applyCodexConfig(path, exe string) error {
	if err := backupOnce(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := splitLines(string(data))
	block := codexBlock(exe)

	start := indexOfLine(lines, "[mcp_servers.agentnet]")
	if start == -1 {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, block...)
	} else {
		end := nextHeader(lines, start+1)
		lines = append(lines[:start:start], append(block, lines[end:]...)...)
	}
	return writeLines(path, lines)
}

// removeCodexConfig deletes our [mcp_servers.agentnet] table from path, if present.
func removeCodexConfig(path string) error {
	if err := backupOnce(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := splitLines(string(data))
	start := indexOfLine(lines, "[mcp_servers.agentnet]")
	if start == -1 {
		return nil
	}
	end := nextHeader(lines, start+1)
	lines = append(lines[:start:start], lines[end:]...)
	return writeLines(path, lines)
}

// splitLines splits s on "\n", dropping one trailing empty line left by a final newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// writeLines joins lines with "\n", terminates with a trailing newline, and writes path.
func writeLines(path string, lines []string) error {
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// indexOfLine returns the index of the first line exactly equal to s, or -1.
func indexOfLine(lines []string, s string) int {
	for i, l := range lines {
		if l == s {
			return i
		}
	}
	return -1
}

// nextHeader returns the index of the next "["-prefixed line at or after from, or len(lines).
func nextHeader(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "[") {
			return i
		}
	}
	return len(lines)
}
