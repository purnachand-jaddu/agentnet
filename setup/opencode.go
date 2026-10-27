package setup

import (
	"embed"
	"encoding/json"
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
			// Both pieces together: "already applied" that ignores missing delivery wiring lies.
			mcp, err := opencodeConfigApplied(configPath, exe)
			if err != nil {
				return false, err
			}
			return mcp && opencodePluginApplied(pluginPath, exe), nil
		},
	}
}

// opencodeJSONC tells whether home uses opencode.jsonc, which edits refuse to touch.
func opencodeJSONC(home string) bool {
	dir := filepath.Join(home, ".config", "opencode")
	_, err := os.Stat(filepath.Join(dir, "opencode.jsonc"))
	return err == nil
}

// opencodeJSONCNoteFor renders the snippet jsonc users get instead of an edit, with the exe
// marshalled like the written config: the note must stay valid JSON for any exe path.
func opencodeJSONCNoteFor(exe string) string {
	snippet, err := json.Marshal(map[string]any{
		"mcp": map[string]any{
			"servers": map[string]any{
				"agentnet": map[string]any{
					"type": "local", "command": []string{exe, "mcp"},
				},
			},
		},
	})
	if err != nil {
		return "" // a marshalling failure here is unreachable: plain maps and strings
	}
	return fmt.Sprintf("opencode: you use opencode.jsonc, which agentnet doesn't edit. Add this object's contents under the top level of opencode.jsonc:\n%s\n", snippet)
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
			fmt.Fprint(os.Stderr, opencodeJSONCNoteFor(exe))
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
	if disabled, _ := entry["disabled"].(bool); disabled {
		return false, nil // opencode won't load it, so it isn't wired
	}
	cmd, ok := entry["command"].([]any)
	if !ok || len(cmd) != 2 || cmd[1] != "mcp" {
		return false, nil
	}
	c0, _ := cmd[0].(string)
	return c0 == exe, nil
}

// applyOpencodePlugin writes the plugin file, embedding the quoted exe path. A file at plugins/agentnet.ts
// that isn't agentnet's is refused, not truncated: --remove keeps strangers' files, so must setup —
// and a file that can't be read can't be proven foreign-or-ours, so it is refused too.
func applyOpencodePlugin(pluginPath, exe string) error {
	data, err := os.ReadFile(pluginPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && !strings.Contains(string(data), opencodePluginMarker) {
		return fmt.Errorf("%s already exists and isn't agentnet's; move or remove it, then re-run `agentnet setup`", pluginPath)
	}
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
// A jsonc home is reported through the jsonc flag, never folded into mcp: agentnet can't know
// whether the manual snippet was ever added, so claiming wired there would print a green that lies.
func OpenCodeWired(home, exe string) (mcp, plugin, jsonc bool, err error) {
	dir := filepath.Join(home, ".config", "opencode")
	mcp, err = opencodeConfigApplied(filepath.Join(dir, "opencode.json"), exe)
	if err != nil {
		return false, false, false, err
	}
	plugin = opencodePluginApplied(filepath.Join(dir, "plugins", "agentnet.ts"), exe)
	jsonc = opencodeJSONC(home)
	return mcp, plugin, jsonc, nil
}
