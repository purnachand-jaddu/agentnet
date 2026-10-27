package setup

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed opencode-plugin.ts
var opencodeFiles embed.FS

// opencodePluginTemplate is `agentnet setup`'s plugin file; AGENTNET_ENGINE_PLACEHOLDER becomes the
// quoted agentnet exe path at apply time.
var opencodePluginTemplate = func() string {
	b, err := opencodeFiles.ReadFile("opencode-plugin.ts")
	if err != nil {
		panic(err)
	}
	return string(b)
}()

const opencodePluginMarker = "agentnet for opencode: written by `agentnet setup`"

// opencodeStep writes the plugin and registers agentnet's MCP server with OpenCode. agentnet never
// edits opencode.jsonc: with one present, Apply prints the snippet to add instead.
func opencodeStep(home, exe string, run Runner) Step {
	dir := filepath.Join(home, ".config", "opencode")
	configPath := filepath.Join(dir, "opencode.json")
	pluginPath := filepath.Join(dir, "plugins", "agentnet.ts")
	return Step{
		Name: "opencode: MCP server and agentnet plugin",
		Apply: func() error {
			errConfig := applyOpencodeConfig(dir, configPath, exe)
			errPlugin := applyOpencodePlugin(pluginPath, exe)
			return errors.Join(errConfig, errPlugin)
		},
		Remove: func() error {
			errConfig := removeOpencodeConfig(dir, configPath)
			errPlugin := removeOpencodePlugin(pluginPath)
			return errors.Join(errConfig, errPlugin)
		},
		Check: func() (bool, error) {
			return opencodeConfigApplied(configPath, exe)
		},
	}
}

// opencodeJSONC tells whether home uses opencode.jsonc, which edits refuse to touch.
func opencodeJSONC(home string) bool {
	dir := filepath.Join(home, ".config", "opencode")
	_, err := os.Stat(filepath.Join(dir, "opencode.jsonc"))
	return err == nil
}

// applyOpencodeConfig writes mcp.servers.agentnet into opencode.json. With a jsonc file and no
// json file it prints what to add instead, since JSONC is hand-edited config the setup doesn't own.
func applyOpencodeConfig(dir, configPath, exe string) error {
	jsoncPath := filepath.Join(dir, "opencode.jsonc")
	_, jsoncErr := os.Stat(jsoncPath)
	if jsoncErr == nil {
		_, jsonErr := os.Stat(configPath)
		if jsonErr != nil {
			if err := backupOnce(jsoncPath); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, opencodeJSONCNote, exe)
			return nil
		}
	}
	if err := backupOnce(configPath); err != nil {
		return err
	}
	doc, err := loadJSON(configPath)
	if err != nil {
		return err
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
		doc["mcp"] = mcp
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["agentnet"] = map[string]any{
		"type":     "local",
		"command":  []string{exe, "mcp"},
		"disabled": false,
	}
	mcp["servers"] = servers
	return saveJSON(configPath, doc)
}

// opencodeJSONCNote is the manual snippet jsonc users get instead of an edit.
const opencodeJSONCNote = `opencode: you use opencode.jsonc, which agentnet doesn't edit. Add under "mcp": {"servers": {"agentnet": {"type": "local", "command": ["%s", "mcp"]}}}
`

// opencodeConfigApplied reads opencode.json directly: agentnet's local MCP server naming exe, or
// not-found, never invoking the opencode binary.
func opencodeConfigApplied(configPath, exe string) (bool, error) {
	doc, err := loadJSON(configPath)
	if err != nil {
		return false, err
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		return false, nil
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, _ := servers["agentnet"].(map[string]any)
	if entry == nil || entry["type"] != "local" {
		return false, nil
	}
	cmd, ok := entry["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[1] != "mcp" {
		return false, nil
	}
	c0, _ := cmd[0].(string)
	return c0 == exe, nil
}

// applyOpencodePlugin writes the plugin file, embedding the quoted exe path.
func applyOpencodePlugin(pluginPath, exe string) error {
	content := strings.Replace(opencodePluginTemplate, "AGENTNET_ENGINE_PLACEHOLDER", strconv.Quote(exe), 1)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(pluginPath, []byte(content), 0o644)
}

// opencodePluginApplied reads the plugin file: ours with this exe, or nothing.
func opencodePluginApplied(pluginPath, exe string) bool {
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), opencodePluginMarker) && strings.Contains(string(data), strconv.Quote(exe))
}

// removeOpencodeConfig deletes agentnet's server entry (and empty mcp/servers containers), except
// when the config lives in an untouchable jsonc file.
func removeOpencodeConfig(dir, configPath string) error {
	jsoncPath := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(jsoncPath); err == nil {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			return nil // nothing we could have written there
		}
	}
	if err := backupOnce(configPath); err != nil {
		return err
	}
	doc, err := loadJSON(configPath)
	if err != nil {
		return err
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		return nil
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, "agentnet")
	if len(servers) == 0 {
		delete(mcp, "servers")
	}
	if len(mcp) == 0 {
		delete(doc, "mcp")
	}
	return saveJSON(configPath, doc)
}

// removeOpencodePlugin deletes the plugin file only when it's agentnet's own.
func removeOpencodePlugin(pluginPath string) error {
	data, err := os.ReadFile(pluginPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), opencodePluginMarker) {
		return nil // a foreign file sharing our name isn't ours to delete
	}
	return os.Remove(pluginPath)
}

// OpenCodeWired reports whether the MCP registration and the plugin are wired for exe, read-only.
// With opencode.jsonc in use the config write is a manual step the person does themselves, so a
// jsonc file and a wired plugin count as wired (check shows what to add by --dry-run).
func OpenCodeWired(home, exe string) (mcp, plugin bool, err error) {
	dir := filepath.Join(home, ".config", "opencode")
	mcp, err = opencodeConfigApplied(filepath.Join(dir, "opencode.json"), exe)
	if err != nil {
		return false, false, err
	}
	if !mcp && opencodeJSONC(home) {
		mcp = true // the snippet is on the person, not on a missing registration we could fix
	}
	plugin = opencodePluginApplied(filepath.Join(dir, "plugins", "agentnet.ts"), exe)
	return mcp, plugin, nil
}
