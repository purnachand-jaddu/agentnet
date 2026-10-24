// Package e2e builds the real agentnet binary and drives it as fake Claude Code and
// Codex processes would, to prove they can exchange messages through one daemon.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// binPath is the built agentnet binary; buildErr is set if the build itself failed.
var (
	binPath  string
	buildErr error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mkdtemp:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "agentnet")
	buildErr = buildAgentnet(binPath)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildAgentnet builds ./cmd/agentnet to out, resolving the module root via `go env GOMOD`.
func buildAgentnet(out string) error {
	modOut, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return fmt.Errorf("go env GOMOD: %w", err)
	}
	modRoot := filepath.Dir(strings.TrimSpace(string(modOut)))

	build := exec.Command("go", "build", "-o", out, "./cmd/agentnet")
	build.Dir = modRoot
	if cout, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, cout)
	}
	return nil
}

// runCLI runs the built binary with env and stdin, capturing stdout/stderr.
func runCLI(env []string, stdin []byte, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(binPath, args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// waitUntil polls check every 50ms until it reports ok, failing the test with the last message after timeout.
func waitUntil(t *testing.T, timeout time.Duration, check func() (ok bool, msg string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, msg := check()
		if ok {
			return
		}
		last = msg
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(last)
}

// fakeClaudeSocket is a Unix socket that collects every newline-delimited frame Claude's inbox would receive.
type fakeClaudeSocket struct {
	mu    sync.Mutex
	lines []string
}

func newFakeClaudeSocket(t *testing.T, path string) *fakeClaudeSocket {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeClaudeSocket{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.readLines(c)
		}
	}()
	return f
}

func (f *fakeClaudeSocket) readLines(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := c.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := string(buf[:i])
				buf = buf[i+1:]
				f.mu.Lock()
				f.lines = append(f.lines, line)
				f.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeClaudeSocket) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// claudeFrame is the subset of an inbox-socket line this test cares about.
type claudeFrame struct {
	Type    string `json:"type"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

// parseClaudeFrame decodes line as a claudeFrame; ok is false if it doesn't parse.
func parseClaudeFrame(line string) (f claudeFrame, ok bool) {
	return f, json.Unmarshal([]byte(line), &f) == nil
}

// shutdownDaemon sends the "shutdown" op over sockPath, then waits for cmd to exit, killing it if it doesn't.
func shutdownDaemon(cmd *exec.Cmd, sockPath string) {
	if c, err := ipc.Dial(sockPath); err == nil {
		c.Call(context.Background(), "shutdown", nil, nil)
		c.Close()
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
	}
}

func TestClaudeAndCodexExchangeMessages(t *testing.T) {
	if buildErr != nil {
		t.Fatalf("agentnet build failed: %v", buildErr)
	}

	tmp, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })

	home := filepath.Join(tmp, "h")
	binDir := filepath.Join(tmp, "bin")
	srcAPI := filepath.Join(tmp, "src", "api")
	srcWeb := filepath.Join(tmp, "src", "web")
	for _, d := range []string{home, binDir, srcAPI, srcWeb} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Config{Person: "alex", Machine: "mbp", Folders: []string{filepath.Join(tmp, "src")}}
	if err := cfg.Save(filepath.Join(home, "config.json")); err != nil {
		t.Fatal(err)
	}

	// Fake codex: a script that logs its argv as one line instead of talking to the real CLI.
	codexLog := filepath.Join(tmp, "codex.log")
	codexScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\n", codexLog)
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// AGENTNET_HOME must stay != $HOME/.agentnet, or autostart would try to kickstart the real launchd service (/bin/launchctl is on PATH).
	env := []string{
		"HOME=" + home,
		"AGENTNET_HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin",
	}

	daemonCmd := exec.Command(binPath, "daemon")
	daemonCmd.Env = env
	logFile, err := os.Create(filepath.Join(tmp, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemonCmd.Stdout, daemonCmd.Stderr = logFile, logFile
	if err := daemonCmd.Start(); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(home, "agentnet.sock")
	t.Cleanup(func() {
		shutdownDaemon(daemonCmd, sockPath)
		logFile.Close()
	})

	// Poll the socket directly, not via a CLI command: any command that dials through
	// autostartDial could itself spawn a second daemon while this one is still starting.
	waitUntil(t, 10*time.Second, func() (bool, string) {
		c, err := ipc.Dial(sockPath)
		if err != nil {
			return false, "daemon socket not ready: " + err.Error()
		}
		c.Close()
		return true, ""
	})

	// Fake Claude: a hook registration carrying the inbox socket, as Claude Code's own hook would.
	claudeSock := filepath.Join(tmp, "c.sock")
	fakeClaude := newFakeClaudeSocket(t, claudeSock)
	claudeEnv := append(append([]string{}, env...), "CLAUDE_CODE_MESSAGING_SOCKET="+claudeSock)
	claudeStdin, _ := json.Marshal(struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}{"c1", srcAPI})
	if _, errOut, err := runCLI(claudeEnv, claudeStdin, "hook", "claude-code", "session-start"); err != nil {
		t.Fatalf("claude hook: %v\n%s", err, errOut)
	}

	// Fake Codex: a hook registration, matching what Codex's own hook would send.
	codexStdin, _ := json.Marshal(struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}{"x1", srcWeb})
	if _, errOut, err := runCLI(env, codexStdin, "hook", "codex", "session-start"); err != nil {
		t.Fatalf("codex hook: %v\n%s", err, errOut)
	}

	if _, errOut, err := runCLI(env, nil, "send", "alex/web-codex", "ping"); err != nil {
		t.Fatalf("send to codex: %v\n%s", err, errOut)
	}
	waitUntil(t, 5*time.Second, func() (bool, string) {
		b, err := os.ReadFile(codexLog)
		if err != nil {
			return false, "codex.log: " + err.Error()
		}
		s := string(b)
		if strings.Contains(s, "queue --thread x1 --message=") && strings.Contains(s, "ping") {
			return true, ""
		}
		return false, "codex.log missing expected push:\n" + s
	})

	if _, errOut, err := runCLI(env, nil, "send", "alex/api-claude", "pong"); err != nil {
		t.Fatalf("send to claude: %v\n%s", err, errOut)
	}
	waitUntil(t, 5*time.Second, func() (bool, string) {
		for _, line := range fakeClaude.snapshot() {
			if f, ok := parseClaudeFrame(line); ok && f.Type == "user" && strings.Contains(f.Message.Content, "pong") {
				return true, ""
			}
		}
		return false, fmt.Sprintf("fake Claude socket never got pong; lines=%v", fakeClaude.snapshot())
	})

	// Give a duplicate or stray push a chance to arrive before asserting there's exactly one.
	time.Sleep(300 * time.Millisecond)
	var pongCount int
	var pongContent string
	for _, line := range fakeClaude.snapshot() {
		f, ok := parseClaudeFrame(line)
		if !ok {
			continue
		}
		if strings.Contains(f.Message.Content, "pong") {
			pongCount++
			pongContent = f.Message.Content
		}
		if strings.Contains(f.Message.Content, "ping") {
			t.Errorf("fake Claude socket received a frame containing ping (codex's message leaked): %s", line)
		}
	}
	if pongCount != 1 {
		t.Fatalf("want exactly one frame containing pong, got %d: %v", pongCount, fakeClaude.snapshot())
	}
	if !strings.Contains(pongContent, `<agentnet-message from="alex/`) {
		t.Errorf("pong frame missing expected framing: %s", pongContent)
	}

	out, errOut, err := runCLI(env, nil, "who")
	if err != nil {
		t.Fatalf("who: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "alex/api-claude") {
		t.Errorf("who output missing alex/api-claude:\n%s", out)
	}
	if !strings.Contains(out, "alex/web-codex") {
		t.Errorf("who output missing alex/web-codex:\n%s", out)
	}
}
