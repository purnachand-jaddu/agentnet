//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// freePort returns a free TCP port on 127.0.0.1 by listening and immediately closing.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// machine is one simulated computer: its own HOME/AGENTNET_HOME, bin dir and LAN port.
type machine struct {
	tmp, home, bin, src string
	env                 []string
}

// newMachine sets up a short-path temp root with its own home, bin and src dirs, and an env where
// every consent prompt auto-approves (that only works because the test binary is built -tags e2e).
func newMachine(t *testing.T, port int, peerPort int) *machine {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	m := &machine{tmp: tmp, home: filepath.Join(tmp, "h"), bin: filepath.Join(tmp, "bin"), src: filepath.Join(tmp, "src")}
	for _, d := range []string{m.home, m.bin, m.src} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m.env = []string{
		"HOME=" + m.home,
		"AGENTNET_HOME=" + m.home,
		"PATH=" + m.bin + ":/usr/bin:/bin",
		"AGENTNET_TEST_APPROVE=yes",
	}
	cfg := config.Config{Port: port, Peers: []string{fmt.Sprintf("lan://127.0.0.1:%d", peerPort)}, Folders: []string{m.src}}
	if err := cfg.Save(filepath.Join(m.home, "config.json")); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *machine) run(stdin []byte, args ...string) (stdout, stderr string, err error) {
	return runCLI(m.env, stdin, args...)
}

func (m *machine) sock() string { return filepath.Join(m.home, "agentnet.sock") }

// startDaemon starts `agentnet daemon` on m, waits for its socket, and registers cleanup to shut it down.
func startDaemon(t *testing.T, m *machine) {
	t.Helper()
	cmd := exec.Command(binPath, "daemon")
	cmd.Env = m.env
	logFile, err := os.Create(filepath.Join(m.tmp, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownDaemon(cmd, m.sock())
		logFile.Close()
	})
	waitUntil(t, 10*time.Second, func() (bool, string) {
		c, err := ipc.Dial(m.sock())
		if err != nil {
			return false, "daemon socket not ready: " + err.Error()
		}
		c.Close()
		return true, ""
	})
}

// TestTwoMachinesExchangeMessages proves the whole chain: real init/join/admit/accept between two
// machines, two daemons talking over the LAN transport, presence, and a remote send delivered into
// a fake Codex on the receiving machine.
func TestTwoMachinesExchangeMessages(t *testing.T) {
	if buildErr != nil {
		t.Fatalf("agentnet build failed: %v", buildErr)
	}

	portA, portB := freePort(t), freePort(t)
	a := newMachine(t, portA, portB)
	b := newMachine(t, portB, portA)

	if _, errOut, err := a.run(nil, "init", "--person", "alex", "--machine", "mbp"); err != nil {
		t.Fatalf("init: %v\n%s", err, errOut)
	}

	joinOut, errOut, err := b.run(nil, "join", "--person", "sam", "--machine", "xps")
	if err != nil {
		t.Fatalf("join: %v\n%s", err, errOut)
	}
	request := strings.SplitN(joinOut, "\n", 2)[0]

	admitOut, errOut, err := a.run(nil, "admit", request)
	if err != nil {
		t.Fatalf("admit: %v\n%s", err, errOut)
	}
	bundle := strings.SplitN(admitOut, "\n", 2)[0]

	if _, errOut, err := b.run(nil, "accept", bundle); err != nil {
		t.Fatalf("accept: %v\n%s", err, errOut)
	}

	startDaemon(t, a)
	startDaemon(t, b)

	// Fake Codex on B: a script that logs its argv, plus the session-start hook codex's own hook would send.
	codexLog := filepath.Join(b.tmp, "codex.log")
	codexScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\n", codexLog)
	if err := os.WriteFile(filepath.Join(b.bin, "codex"), []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	srcWeb := filepath.Join(b.src, "web")
	if err := os.MkdirAll(srcWeb, 0o700); err != nil {
		t.Fatal(err)
	}
	codexStdin, _ := json.Marshal(struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}{"x1", srcWeb})
	if _, errOut, err := b.run(codexStdin, "hook", "codex", "session-start"); err != nil {
		t.Fatalf("codex hook: %v\n%s", err, errOut)
	}

	waitUntil(t, 15*time.Second, func() (bool, string) {
		out, _, err := a.run(nil, "who")
		if err != nil {
			return false, "who: " + err.Error()
		}
		if strings.Contains(out, "sam/web-codex") {
			return true, ""
		}
		return false, "who missing sam/web-codex:\n" + out
	})

	out, errOut, err := a.run(nil, "send", "sam/web-codex", "hi")
	if err != nil {
		t.Fatalf("send: %v\n%s", err, errOut)
	}
	if strings.Contains(out, "refused") {
		t.Fatalf("send receipt says refused: %s", out)
	}

	waitUntil(t, 10*time.Second, func() (bool, string) {
		b, err := os.ReadFile(codexLog)
		if err != nil {
			return false, "codex.log: " + err.Error()
		}
		s := string(b)
		if strings.Contains(s, "queue --thread x1 --message=") && strings.Contains(s, "hi") {
			return true, ""
		}
		return false, "codex.log missing expected push:\n" + s
	})
}
