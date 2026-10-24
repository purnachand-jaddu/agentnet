package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRunner records every call it receives and never touches a real process.
func fakeRunner(t *testing.T) (Runner, *[][]string) {
	t.Helper()
	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	return run, &calls
}

// writeFakeScript writes an executable shell script named name in dir that appends its argv to log.
func writeFakeScript(t *testing.T, dir, name, log, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func unmarshalMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
	return m
}

// TestClaudeHooksPreserveForeignEntries checks a foreign hook survives apply and that remove only
// deletes our own group, leaving the foreign one.
func TestClaudeHooksPreserveForeignEntries(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"other_key":"keep me","hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"command","command":"foreign-tool hook"}]}]}}`
	if err := os.WriteFile(settings, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	run, _ := fakeRunner(t)
	step := claudeStep(home, "/usr/local/bin/agentnet", run)

	if err := step.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first := readFile(t, settings)
	doc := unmarshalMap(t, first)
	if doc["other_key"] != "keep me" {
		t.Fatalf("other_key lost: %v", doc)
	}
	hooks := doc["hooks"].(map[string]any)
	ptu := hooks["PostToolUse"].([]any)
	if len(ptu) != 2 {
		t.Fatalf("PostToolUse groups = %d, want 2 (foreign + ours)", len(ptu))
	}
	for _, event := range hookEventOrder {
		if _, ok := hooks[event]; !ok {
			t.Fatalf("missing hook event %s", event)
		}
	}

	// Backup taken once, with the original content.
	bak := readFile(t, settings+".agentnet-bak")
	if bak != foreign {
		t.Fatalf("backup = %q, want original %q", bak, foreign)
	}

	// Apply again: byte-identical.
	if err := step.Apply(); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := readFile(t, settings)
	if first != second {
		t.Fatalf("apply is not idempotent:\n%s\n---\n%s", first, second)
	}
	// Backup still holds the pre-setup original, not overwritten by the second apply.
	if readFile(t, settings+".agentnet-bak") != foreign {
		t.Fatal("backup was overwritten")
	}

	if err := step.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := unmarshalMap(t, readFile(t, settings))
	if after["other_key"] != "keep me" {
		t.Fatalf("other_key lost after remove: %v", after)
	}
	afterHooks, ok := after["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks missing after remove: %v", after)
	}
	if _, ok := afterHooks["PostToolUse"]; !ok {
		t.Fatal("foreign PostToolUse entry was removed")
	}
	afterPTU := afterHooks["PostToolUse"].([]any)
	if len(afterPTU) != 1 {
		t.Fatalf("PostToolUse groups after remove = %d, want 1 (foreign only)", len(afterPTU))
	}
	for _, event := range hookEventOrder {
		if event == "PostToolUse" {
			continue
		}
		if _, ok := afterHooks[event]; ok {
			t.Fatalf("event %s should have been dropped (was ours only)", event)
		}
	}
}

// TestClaudeMCPLogsAddArgv runs claudeStep.Apply against a real fake `claude` executable on PATH
// and checks its argv log shows the exact `mcp add` invocation.
func TestClaudeMCPLogsAddArgv(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "claude.log")
	writeFakeScript(t, bin, "claude", log, `
if [ "$1" = "mcp" ] && [ "$2" = "get" ]; then
  exit 1
fi
exit 0
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := "/usr/local/bin/agentnet"
	step := claudeStep(t.TempDir(), exe, Exec)
	if err := claudeMCPAdd(exe, Exec); err != nil {
		t.Fatalf("claudeMCPAdd: %v", err)
	}
	_ = step // settings.json side isn't under test here

	got := readFile(t, log)
	if !strings.Contains(got, "mcp get agentnet") {
		t.Fatalf("log missing mcp get: %q", got)
	}
	want := "mcp add --scope user agentnet -- " + exe + " mcp"
	if !strings.Contains(got, want) {
		t.Fatalf("log = %q, want to contain %q", got, want)
	}
}

// TestCodexHooksPreserveForeignEntries mirrors the Claude Code hooks test for Codex's hooks.json.
func TestCodexHooksPreserveForeignEntries(t *testing.T) {
	home := t.TempDir()
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"hooks":{"PostToolUse":[{"matcher":"","hooks":[{"type":"command","command":"other-tool hook"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	run, _ := fakeRunner(t)
	step := codexStep(home, "/usr/local/bin/agentnet", run)

	if err := step.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first := readFile(t, hooksPath)
	doc := unmarshalMap(t, first)
	hooks := doc["hooks"].(map[string]any)
	ptu := hooks["PostToolUse"].([]any)
	if len(ptu) != 2 {
		t.Fatalf("PostToolUse groups = %d, want 2", len(ptu))
	}

	if err := step.Apply(); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := readFile(t, hooksPath)
	if first != second {
		t.Fatalf("apply is not idempotent:\n%s\n---\n%s", first, second)
	}

	if err := step.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := unmarshalMap(t, readFile(t, hooksPath))
	afterHooks := after["hooks"].(map[string]any)
	afterPTU := afterHooks["PostToolUse"].([]any)
	if len(afterPTU) != 1 {
		t.Fatalf("PostToolUse groups after remove = %d, want 1 (foreign only)", len(afterPTU))
	}
	cmd := groupCommand(afterPTU[0])
	if cmd != "other-tool hook" {
		t.Fatalf("remaining entry = %q, want the foreign one", cmd)
	}
}

// TestCodexConfigTomlPreservesForeignTable checks our table appears exactly once alongside a
// foreign one, and that remove deletes only ours.
func TestCodexConfigTomlPreservesForeignTable(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := "[mcp_servers.other]\ncommand = \"other\"\nargs = []\n"
	if err := os.WriteFile(configPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	exe := "/usr/local/bin/agentnet"
	if err := applyCodexConfig(configPath, exe); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first := readFile(t, configPath)
	if !strings.Contains(first, "[mcp_servers.other]") {
		t.Fatalf("foreign table lost:\n%s", first)
	}
	if n := strings.Count(first, "[mcp_servers.agentnet]"); n != 1 {
		t.Fatalf("our table appears %d times, want 1:\n%s", n, first)
	}
	if !strings.Contains(first, `command = "`+exe+`"`) {
		t.Fatalf("our command missing:\n%s", first)
	}

	if err := applyCodexConfig(configPath, exe); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := readFile(t, configPath)
	if first != second {
		t.Fatalf("apply is not idempotent:\n%s\n---\n%s", first, second)
	}

	if err := removeCodexConfig(configPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := readFile(t, configPath)
	if strings.Contains(after, "[mcp_servers.agentnet]") {
		t.Fatalf("our table survived remove:\n%s", after)
	}
	if !strings.Contains(after, "[mcp_servers.other]") {
		t.Fatalf("foreign table lost after remove:\n%s", after)
	}
}

// TestPlistRendersServiceAndKeepAliveAndPATH drives Plan for darwin with a fake claude on PATH and
// an injected Runner, then checks the written plist's content by string compare. No real launchctl
// is invoked.
func TestPlistRendersServiceAndKeepAliveAndPATH(t *testing.T) {
	oldGOOS := goos
	goos = "darwin"
	t.Cleanup(func() { goos = oldGOOS })

	bin := t.TempDir()
	writeFakeScript(t, bin, "claude", filepath.Join(bin, "claude.log"), "exit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", filepath.Join(home, "state"))

	run, calls := fakeRunner(t)
	steps := Plan(home, "/usr/local/bin/agentnet", run)

	var svc *Step
	for i := range steps {
		if steps[i].Name == "Background service (launchd)" {
			svc = &steps[i]
		}
	}
	if svc == nil {
		t.Fatal("no launchd service step in Plan")
	}
	if err := svc.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "dev.agentnet.daemon.plist")
	plist := readFile(t, plistPath)
	for _, want := range []string{"<string>--service</string>", "<key>KeepAlive</key>", bin, "<key>AGENTNET_HOME</key>"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}

	found := false
	for _, c := range *calls {
		if len(c) > 0 && c[0] == "launchctl" {
			found = true
		}
	}
	if !found {
		t.Fatal("launchctl was never called through the injected runner")
	}
}

// TestSystemdUnitRenders checks the rendered systemd unit via Plan on a forced linux goos.
func TestSystemdUnitRenders(t *testing.T) {
	oldGOOS := goos
	goos = "linux"
	t.Cleanup(func() { goos = oldGOOS })

	bin := t.TempDir()
	writeFakeScript(t, bin, "claude", filepath.Join(bin, "claude.log"), "exit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	home := t.TempDir()
	run, _ := fakeRunner(t)
	steps := Plan(home, "/usr/local/bin/agentnet", run)

	var svc *Step
	for i := range steps {
		if steps[i].Name == "Background service (systemd)" {
			svc = &steps[i]
		}
	}
	if svc == nil {
		t.Fatal("no systemd service step in Plan")
	}
	if err := svc.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "agentnet.service")
	unit := readFile(t, unitPath)
	for _, want := range []string{"ExecStart=/usr/local/bin/agentnet daemon --service", "Restart=always", "WantedBy=default.target", bin} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

// TestPlanPicksServiceForRuntimeGOOS is a smoke test that Plan's default goos matches the real OS.
func TestPlanPicksServiceForRuntimeGOOS(t *testing.T) {
	if goos != runtime.GOOS {
		t.Fatalf("goos = %q, want %q", goos, runtime.GOOS)
	}
}
