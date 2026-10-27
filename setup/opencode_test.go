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

// TestOpencodeWiredReportsJSONCSeparately mirrors the claude/codex wired checks: no files means
// not wired; an applied config plus plugin is wired; a jsonc home means the config side is a
// manual step, never claimed as done.
func TestOpencodeWired(t *testing.T) {
	home := t.TempDir()
	if mcp, plugin, jsonc, err := OpenCodeWired(home, "/exe"); mcp || plugin || jsonc || err != nil {
		t.Fatalf("empty home: mcp=%v plugin=%v jsonc=%v err=%v", mcp, plugin, jsonc, err)
	}
	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	mcp, plugin, jsonc, err := OpenCodeWired(home, "/exe")
	if err != nil {
		t.Fatal(err)
	}
	if !mcp || !plugin || jsonc {
		t.Fatalf("after apply: mcp=%v plugin=%v jsonc=%v", mcp, plugin, jsonc)
	}
	// A foreign, wrong-exe entry is not ours.
	other := t.TempDir()
	if err := opencodeStep(other, "/other/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	if mcp, plugin, jsonc, err := OpenCodeWired(other, "/exe"); mcp || plugin || jsonc || err != nil {
		t.Fatalf("stranger's home: mcp=%v plugin=%v jsonc=%v err=%v", mcp, plugin, jsonc, err)
	}
}

// TestOpencodeWiredRejectsDisabledEntry pins that a disabled agentnet entry is not "wired": opencode
// won't load it, so claiming the MCP side would print a green that lies.
func TestOpencodeWiredRejectsDisabledEntry(t *testing.T) {
	home := t.TempDir()
	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disabled := strings.Replace(string(data), `"disabled": false`, `"disabled": true`, 1)
	if disabled == string(data) {
		t.Fatalf("expected a disabled:false in the written config: %s", data)
	}
	if err := os.WriteFile(path, []byte(disabled), 0o644); err != nil {
		t.Fatal(err)
	}
	mcp, _, _, err := OpenCodeWired(home, "/exe")
	if err != nil {
		t.Fatal(err)
	}
	if mcp {
		t.Fatal("a disabled entry must not count as wired")
	}
}

// TestOpencodeWiredJSONCIsManualNotWired pins that a jsonc home is reported as a manual step, not
// as wired: agentnet cannot know whether the snippet was ever added.
func TestOpencodeWiredJSONCIsManualNotWired(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte("{\"plugin\":[\"x\"] // note\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	mcp, plugin, jsonc, err := OpenCodeWired(home, "/exe")
	if err != nil {
		t.Fatal(err)
	}
	if mcp || !plugin || !jsonc {
		t.Fatalf("jsonc-only home: mcp=%v plugin=%v jsonc=%v", mcp, plugin, jsonc)
	}
}

// TestOpencodeApplyRefusesForeignPluginFile checks apply never destroys a plugins/agentnet.ts that
// isn't agentnet's: --remove already keeps it, ordinary setup must not truncate it either.
func TestOpencodeApplyRefusesForeignPluginFile(t *testing.T) {
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

	err := opencodeStep(home, "/exe", Exec).Apply()
	if err == nil || !strings.Contains(err.Error(), "agentnet.ts") {
		t.Fatalf("apply should refuse a foreign plugin file, got: %v", err)
	}
	data, err2 := os.ReadFile(path)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(data) != foreign {
		t.Fatal("foreign plugin file was modified")
	}
}

// TestOpencodeApplyRefusesUnreadablePluginFile pins that a file agentnet can't read is treated as
// foreign (never silently overwritten): the README's setup-refusal claim counts on this.
func TestOpencodeApplyRefusesUnreadablePluginFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugins", "agentnet.ts")
	if err := os.WriteFile(path, []byte("unreadable foreign plugin"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil { // unreadable, so ownership can't be proven
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if err := opencodeStep(home, "/exe", Exec).Apply(); err == nil {
		t.Fatal("apply should refuse a plugin file it cannot inspect")
	}
	// The point of the refusal: an uninspectable file must come out of setup byte-identical. Make
	// it readable again before verifying that.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "unreadable foreign plugin" {
		t.Fatalf("unreadable plugin file must be untouched: %v %s", err, data)
	}
}

// TestOpencodeBothFilesPrefersJSON: with opencode.json AND opencode.jsonc already present, setup
// writes the parseable json file (the jsonc side stays the person's hand-edited file, untouched).
func TestOpencodeBothFilesPrefersJSON(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonc := "{\"plugin\":[\"x\"] // note\n}"
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(jsonc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{\"theme\":\"x\"}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := opencodeStep(home, "/exe", Exec).Apply(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "opencode.jsonc")); err != nil || string(data) != jsonc {
		t.Fatalf("opencode.jsonc must be untouched: %v %s", err, data)
	}
	data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	servers, _ := doc["mcp"].(map[string]any)["servers"].(map[string]any)
	entry, _ := servers["agentnet"].(map[string]any)
	if entry == nil || entry["type"] != "local" {
		t.Fatalf("opencode.json must carry agentnet's server entry, got %v", doc)
	}
	if cmd, _ := entry["command"].([]any); len(cmd) != 2 || cmd[0] != "/exe" {
		t.Fatalf("opencode.json's agentnet entry must name exe mcp, got %v", entry["command"])
	}
}

// TestOpencodeCheckSeesBothPieces: a step whose config is applied but whose plugin was removed
// (or never applied) must not read as already applied.
func TestOpencodeCheckSeesBothPieces(t *testing.T) {
	home := t.TempDir()
	s := opencodeStep(home, "/exe", Exec)
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Check(); err != nil || !ok {
		t.Fatalf("check after apply: %v %v", ok, err)
	}
	if err := os.Remove(filepath.Join(home, ".config", "opencode", "plugins", "agentnet.ts")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Check(); err != nil || ok {
		t.Fatalf("check without the plugin: %v %v", ok, err)
	}
}

// TestOpencodeJSONCNoteEscapesExe: the snippet printed for jsonc users must be valid JSON for
// whatever exe path it carries, escaped like the written config is.
func TestOpencodeJSONCNoteEscapesExe(t *testing.T) {
	exe := `/tmp/w Space"quote\agentnet`
	note := opencodeJSONCNoteFor(exe)
	// The exe must appear escaped exactly as JSON escapes it, not broken by the raw characters.
	raw, _ := json.Marshal(exe)
	if !strings.Contains(note, string(raw)) {
		t.Fatalf("note lost the exe entirely:\n%s", note)
	}
	// The note embeds a JSON object under mcp.servers.agentnet; parse just that part.
	start, end := strings.Index(note, "{"), strings.LastIndex(note, "}")
	if start < 0 || end <= start {
		t.Fatalf("note carries no JSON object:\n%s", note)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(note[start:end+1]), &parsed); err != nil {
		t.Fatalf("the note's object is not valid JSON: %v\n%s", err, note)
	}
	mcp, _ := parsed["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	entry, _ := servers["agentnet"].(map[string]any)
	cmd, _ := entry["command"].([]any)
	if len(cmd) != 2 || cmd[0] != exe {
		t.Fatalf("note's command escapes wrongly: %v", entry)
	}
}
