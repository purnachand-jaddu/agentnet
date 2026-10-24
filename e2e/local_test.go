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

// binPath is the built agentnet binary; buildErr is set if the build failed, so tests skip instead of failing.
var (
	binPath  string
	buildErr error
)

func TestMain(m *testing.M) {
	buildErr = buildAgentnet()
	os.Exit(m.Run())
}

// buildAgentnet builds ./cmd/agentnet into a temp dir, resolving the module root via `go env GOMOD`.
func buildAgentnet() error {
	modOut, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return fmt.Errorf("go env GOMOD: %w", err)
	}
	modRoot := filepath.Dir(strings.TrimSpace(string(modOut)))

	tmp, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		return err
	}
	binPath = filepath.Join(tmp, "agentnet")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/agentnet")
	build.Dir = modRoot
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, out)
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
		t.Skipf("agentnet build failed: %v", buildErr)
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

	waitUntil(t, 10*time.Second, func() (bool, string) {
		_, errOut, err := runCLI(env, nil, "who")
		if err != nil {
			return false, fmt.Sprintf("daemon not ready: %v: %s", err, errOut)
		}
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
			var frame struct {
				Type    string `json:"type"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &frame) == nil && frame.Type == "user" && strings.Contains(frame.Message.Content, "pong") {
				return true, ""
			}
		}
		return false, fmt.Sprintf("fake Claude socket never got pong; lines=%v", fakeClaude.snapshot())
	})

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
