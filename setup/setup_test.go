package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
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

func findStep(t *testing.T, steps []Step, name string) Step {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step named %q", name)
	return Step{}
}

// fakeDaemon is a minimal real ipc server at paths.Socket() that answers "shutdown" by stopping
// itself (closing its listener), and records whether it was ever asked to.
type fakeDaemon struct {
	mu   sync.Mutex
	got  bool
	done chan struct{}
}

func startFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDaemon{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	handler := func(_ context.Context, _ ipc.Conn, op string, _ json.RawMessage) (any, error) {
		if op == "shutdown" {
			fd.mu.Lock()
			fd.got = true
			fd.mu.Unlock()
			cancel()
		}
		return nil, nil
	}
	go func() {
		ipc.Serve(ctx, l, handler, nil)
		close(fd.done)
	}()
	t.Cleanup(func() { cancel(); <-fd.done })
	return fd
}

func (fd *fakeDaemon) gotShutdown() bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.got
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

// TestClaudeMCPApplyLogsRemoveThenAddArgv runs claudeMCPApply against a real fake `claude`
// executable on PATH and checks its argv log shows remove, then the exact `mcp add` invocation.
// This never calls `claude mcp get`, which spawns the configured server as a health check.
func TestClaudeMCPApplyLogsRemoveThenAddArgv(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "claude.log")
	writeFakeScript(t, bin, "claude", log, "exit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	exe := "/usr/local/bin/agentnet"
	if err := claudeMCPApply(exe, Exec); err != nil {
		t.Fatalf("claudeMCPApply: %v", err)
	}

	got := readFile(t, log)
	if !strings.Contains(got, "mcp remove --scope user agentnet") {
		t.Fatalf("log missing mcp remove: %q", got)
	}
	if strings.Contains(got, "mcp get") {
		t.Fatalf("claudeMCPApply must never call `claude mcp get`: %q", got)
	}
	want := "mcp add --scope user agentnet -- " + exe + " mcp"
	if !strings.Contains(got, want) {
		t.Fatalf("log = %q, want to contain %q", got, want)
	}
}

// TestClaudeRemoveTwiceIsIdempotent checks that a "not registered" failure from `claude mcp
// remove` doesn't stop the hooks half from running, on either call.
func TestClaudeRemoveTwiceIsIdempotent(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "claude" && len(args) > 1 && args[1] == "remove" {
			return []byte(`No MCP server named "agentnet" in user scope`), errors.New("exit status 1")
		}
		return nil, nil
	}
	step := claudeStep(home, "/usr/local/bin/agentnet", run)

	if err := step.Remove(); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := step.Remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	doc := unmarshalMap(t, readFile(t, settings))
	if _, ok := doc["hooks"]; ok {
		t.Fatalf("hooks key should be gone: %v", doc)
	}
}

// TestClaudeCheckReadsClaudeJSON checks Check reads <home>/.claude.json directly and never
// invokes the claude binary (which `claude mcp get` would, as a health check that spawns it).
func TestClaudeCheckReadsClaudeJSON(t *testing.T) {
	home := t.TempDir()
	exe := "/usr/local/bin/agentnet"
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"mcpServers":{"agentnet":{"command":"`+exe+`"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run, calls := fakeRunner(t)
	step := claudeStep(home, exe, run)

	applied, err := step.Check()
	if err != nil || !applied {
		t.Fatalf("Check = %v, %v, want true, nil", applied, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("Check invoked an external command: %v", *calls)
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

// TestCodexRemoveTwiceIsIdempotent checks --remove is safe to run twice on Codex's files.
func TestCodexRemoveTwiceIsIdempotent(t *testing.T) {
	home := t.TempDir()
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	configPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[mcp_servers.agentnet]\ncommand = \"x\"\nargs = [\"mcp\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	run, _ := fakeRunner(t)
	step := codexStep(home, "/usr/local/bin/agentnet", run)

	if err := step.Remove(); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := step.Remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if strings.Contains(readFile(t, configPath), "[mcp_servers.agentnet]") {
		t.Fatal("our table survived two removes")
	}
}

// TestExeWithSpaceIsQuotedEverywhere checks the hook command, config.toml, and the systemd
// ExecStart all handle an exe path containing a space.
func TestExeWithSpaceIsQuotedEverywhere(t *testing.T) {
	exe := "/Applications/My App/agentnet"
	home := t.TempDir()

	hooksPath := filepath.Join(home, "hooks.json")
	if err := applyHooks(hooksPath, exe, claudeAgent); err != nil {
		t.Fatal(err)
	}
	quotedExe := shellQuoteIfNeeded(exe)
	if !strings.HasPrefix(quotedExe, "'") {
		t.Fatalf("shellQuoteIfNeeded(%q) = %q, want it single-quoted", exe, quotedExe)
	}
	if !strings.Contains(readFile(t, hooksPath), quotedExe+" hook claude-code") {
		t.Fatalf("hook command not quoted:\n%s", readFile(t, hooksPath))
	}

	configPath := filepath.Join(home, "config.toml")
	if err := applyCodexConfig(configPath, exe); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, configPath), `command = "`+exe+`"`) {
		t.Fatalf("config.toml command not quoted:\n%s", readFile(t, configPath))
	}

	quotedForUnit := systemdQuoteIfNeeded(exe)
	if !strings.HasPrefix(quotedForUnit, `"`) {
		t.Fatalf("systemdQuoteIfNeeded(%q) = %q, want it double-quoted", exe, quotedForUnit)
	}
	unit := systemdUnit(exe, nil)
	if !strings.Contains(unit, "ExecStart="+quotedForUnit+" daemon --service") {
		t.Fatalf("ExecStart not quoted:\n%s", unit)
	}
}

// TestSaveJSONDoesNotHTMLEscape checks a foreign value with shell-ish characters round-trips
// byte-for-byte, i.e. json.Encoder's default HTML-escaping is disabled.
func TestSaveJSONDoesNotHTMLEscape(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"note":"a && b > c"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyHooks(settings, "/usr/local/bin/agentnet", claudeAgent); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, settings)
	if !strings.Contains(got, "a && b > c") {
		t.Fatalf("foreign value was re-escaped:\n%s", got)
	}
}

// TestPlistRendersServiceAndKeepAliveAndPATH drives Plan for darwin with a fake claude on PATH;
// the fake Runner starts a real (but minimal) fake daemon when "bootstrap" runs, so the step's
// post-start wait for the socket succeeds without ever calling real launchctl.
func TestPlistRendersServiceAndKeepAliveAndPATH(t *testing.T) {
	oldGOOS := goos
	goos = "darwin"
	t.Cleanup(func() { goos = oldGOOS })

	bin := t.TempDir()
	writeFakeScript(t, bin, "claude", filepath.Join(bin, "claude.log"), "exit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", filepath.Join(home, "state"))

	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
			startFakeDaemon(t)
		}
		return nil, nil
	}
	steps := Plan(home, "/usr/local/bin/agentnet", run)
	svc := findStep(t, steps, "Background service (launchd)")

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

	var seq []string
	for _, c := range calls {
		if c[0] == "launchctl" {
			seq = append(seq, c[1])
		}
	}
	if len(seq) != 2 || seq[0] != "bootout" || seq[1] != "bootstrap" {
		t.Fatalf("launchctl calls = %v, want [bootout bootstrap]", seq)
	}
}

// TestMacServiceReapplyRunsBootoutThenBootstrap checks a second Apply (re-run/upgrade) also
// goes through bootout then bootstrap, taking over the still-running daemon from the first Apply.
func TestMacServiceReapplyRunsBootoutThenBootstrap(t *testing.T) {
	oldGOOS := goos
	goos = "darwin"
	t.Cleanup(func() { goos = oldGOOS })

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", filepath.Join(home, "state"))

	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
			startFakeDaemon(t)
		}
		return nil, nil
	}
	steps := Plan(home, "/usr/local/bin/agentnet", run)
	svc := findStep(t, steps, "Background service (launchd)")

	if err := svc.Apply(); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := svc.Apply(); err != nil {
		t.Fatalf("second apply (re-run): %v", err)
	}

	var seq []string
	for _, c := range calls {
		if c[0] == "launchctl" {
			seq = append(seq, c[1])
		}
	}
	want := []string{"bootout", "bootstrap", "bootout", "bootstrap"}
	if len(seq) != len(want) {
		t.Fatalf("launchctl calls = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("launchctl calls = %v, want %v", seq, want)
		}
	}
}

// TestSystemdUnitRenders checks the rendered systemd unit via Plan on a forced linux goos, with
// Codex detected only via ~/.codex (no codex binary on PATH) — the common bin dirs must still
// appear in PATH.
func TestSystemdUnitRenders(t *testing.T) {
	oldGOOS := goos
	goos = "linux"
	t.Cleanup(func() { goos = oldGOOS })

	bin := t.TempDir()
	writeFakeScript(t, bin, "claude", filepath.Join(bin, "claude.log"), "exit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", filepath.Join(home, "state"))
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "systemctl" && len(args) > 1 && args[1] == "restart" {
			startFakeDaemon(t)
		}
		return nil, nil
	}
	steps := Plan(home, "/usr/local/bin/agentnet", run)
	svc := findStep(t, steps, "Background service (systemd)")

	if err := svc.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "agentnet.service")
	unit := readFile(t, unitPath)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/agentnet daemon --service", "Restart=always", "WantedBy=default.target",
		bin, "/usr/bin", "Environment=AGENTNET_HOME=",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}

	var seq []string
	for _, c := range calls {
		if c[0] == "systemctl" {
			seq = append(seq, c[2])
		}
	}
	want := []string{"daemon-reload", "enable", "restart"}
	if len(seq) != len(want) {
		t.Fatalf("systemctl calls = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("systemctl calls = %v, want %v", seq, want)
		}
	}
}

// TestTakeOverRunningDaemonSendsShutdown drives takeOverRunningDaemon against a real (but
// minimal, test-local) fake daemon and checks it both received "shutdown" and actually stopped
// answering afterward.
func TestTakeOverRunningDaemonSendsShutdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	fd := startFakeDaemon(t)

	takeOverRunningDaemon()

	if !fd.gotShutdown() {
		t.Fatal("shutdown op was never sent")
	}
	if !waitForSocket(2*time.Second, false) {
		t.Fatal("socket still answers after shutdown")
	}
}

// TestPlanPicksServiceForRuntimeGOOS is a smoke test that Plan's default goos matches the real OS.
func TestPlanPicksServiceForRuntimeGOOS(t *testing.T) {
	if goos != runtime.GOOS {
		t.Fatalf("goos = %q, want %q", goos, runtime.GOOS)
	}
}
