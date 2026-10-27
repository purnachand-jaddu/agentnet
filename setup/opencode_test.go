package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func readOpencodeJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("opencode.json is not valid JSON: %v", err)
	}
	return doc
}

// TestOpencodeApplyWritesConfigAndPlugin checks that Apply registers the MCP server under
// mcp.servers.agentnet (an exe with spaces is safe: it travels as a JSON array) and writes the
// plugin file with the exe path embedded.
func TestOpencodeApplyWritesConfigAndPlugin(t *testing.T) {
	home := t.TempDir()
	exe := "/Applications/My Tools/agentnet"
	s := opencodeStep(home, exe, Exec)
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}

	doc := readOpencodeJSON(t, home)
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		t.Fatal("mcp key missing after apply")
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		t.Fatalf("mcp.servers missing: %v", doc)
	}
	entry, _ := servers["agentnet"].(map[string]any)
	if entry == nil || entry["type"] != "local" {
		t.Fatalf("agentnet server entry = %v", entry)
	}
	cmd, _ := entry["command"].([]any)
	if len(cmd) != 2 || cmd[0] != exe || cmd[1] != "mcp" {
		t.Fatalf("agentnet command = %v", cmd)
	}

	pluginData, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "plugins", "agentnet.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pluginData), "written by `agentnet setup`") {
		t.Fatal("plugin file missing its marker")
	}
	if !strings.Contains(string(pluginData), exe) {
		t.Fatal("plugin file lacks the exe path")
	}
}

// TestOpencodeApplyPreservesForeignKeysAndServers checks that unrelated config content and other
// people's MCP servers survive an apply.
func TestOpencodeApplyPreservesForeignKeysAndServers(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"model":"claude-x","mcp":{"servers":{"other":{"type":"local","command":["x"]}}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	doc := readOpencodeJSON(t, home)
	if doc["model"] != "claude-x" {
		t.Fatalf("foreign key lost: %v", doc)
	}
	servers := doc["mcp"].(map[string]any)["servers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("foreign MCP server lost: %v", servers)
	}
	if _, ok := servers["agentnet"]; !ok {
		t.Fatalf("agentnet entry missing: %v", servers)
	}
}

// TestOpencodeApplyIsIdempotent checks that applying twice changes nothing the second time.
func TestOpencodeApplyIsIdempotent(t *testing.T) {
	home := t.TempDir()
	s := opencodeStep(home, "/exe", Exec)
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second apply changed the config:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestOpencodeApplySkipsJSONCConfigWithPlugin checks the reason-for-jsonc contract: agentnet never
// edits opencode.jsonc, so with a jsonc file and no json file, Apply writes only the plugin file.
func TestOpencodeApplySkipsJSONCConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte("{\"plugin\":[\"x\"] // a comment\n}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("opencode.json should not be created next to a jsonc file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "agentnet.ts")); err != nil {
		t.Fatalf("plugin file should still be written: %v", err)
	}
}

// TestOpencodeRemoveCleansOwnEntries checks that remove prunes only agentnet's entry and plugin
// file, keeps foreign content, and drops empty mcp/servers containers.
func TestOpencodeRemoveCleansOwnEntries(t *testing.T) {
	home := t.TempDir()
	exe := "/exe"
	if err := opencodeStep(home, exe, Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	// A foreign server should survive: apply once more with foreign content present.
	dir := filepath.Join(home, ".config", "opencode")
	doc := readOpencodeJSON(t, home)
	doc["mcp"].(map[string]any)["servers"].(map[string]any)["other"] = map[string]any{"type": "local", "command": []string{"x"}}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeStep(home, exe, Exec).Remove(); err != nil {
		t.Fatal(err)
	}
	doc = readOpencodeJSON(t, home)
	mcp, _ := doc["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	if _, ok := servers["agentnet"]; ok {
		t.Fatalf("agentnet entry should be removed: %v", doc)
	}
	if _, ok := servers["other"]; !ok {
		t.Fatalf("foreign server lost: %v", doc)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "agentnet.ts")); !os.IsNotExist(err) {
		t.Fatalf("plugin file should be removed: %v", err)
	}
}

// TestOpencodeRemoveKeepsForeignPluginFile checks remove never deletes a plugins/agentnet.ts that
// isn't ours.
func TestOpencodeRemoveKeepsForeignPluginFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := "// someone else's plugin with the same name\nexport default {}"
	path := filepath.Join(dir, "plugins", "agentnet.ts")
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeStep(home, "/exe", Exec).Remove(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != foreign {
		t.Fatal("foreign plugin file was modified or deleted")
	}
}

// TestOpencodePluginEngineIsAQuotedPath exercises the template substitution: the exe must land
// inside a valid TypeScript string literal (opencode parses the file with bun), escaped if it
// contains quotes or backslashes.
func TestOpencodePluginEngineIsAQuotedPath(t *testing.T) {
	home := t.TempDir()
	exe := `/tmp/w Space"quote\agentnet`
	if err := opencodeStep(home, exe, Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "plugins", "agentnet.ts"))
	if err != nil {
		t.Fatal(err)
	}
	want := `const ENGINE = ` + strconv.Quote(exe)
	if !strings.Contains(string(data), want) {
		t.Fatalf("plugin lacks a valid engine literal:\nwant: %s\nfile:\n%s", want, data)
	}
}

// TestOpencodeWiredChecksBothPieces mirrors the claude/codex wired checks: no files means not
// wired, an applied config plus plugin means wired.
func TestOpencodeWired(t *testing.T) {
	home := t.TempDir()
	if mcp, plugin, err := OpenCodeWired(home, "/exe"); mcp || plugin || err != nil {
		t.Fatalf("empty home: mcp=%v plugin=%v err=%v", mcp, plugin, err)
	}
	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	mcp, plugin, err := OpenCodeWired(home, "/exe")
	if err != nil {
		t.Fatal(err)
	}
	if !mcp || !plugin {
		t.Fatalf("after apply: mcp=%v plugin=%v", mcp, plugin)
	}
	// A foreign, wrong-exe entry is not ours.
	other := t.TempDir()
	if err := opencodeStep(other, "/other/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	if mcp, plugin, err := OpenCodeWired(other, "/exe"); mcp || plugin || err != nil {
		t.Fatalf("stranger's home: mcp=%v plugin=%v err=%v", mcp, plugin, err)
	}
}
