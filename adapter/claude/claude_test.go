package claude

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

// sockPath creates a short Unix socket path within macOS's 104-byte sun_path limit.
func sockPath(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestPostInboxWritesAuthThenUserFrame(t *testing.T) {
	sock := sockPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []string, 1)
	go func() {
		c, _ := l.Accept()
		defer c.Close()
		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()
	if err := PostInbox(sock, "tok", "hello"); err != nil {
		t.Fatal(err)
	}
	lines := <-got
	if len(lines) != 2 || !strings.Contains(lines[0], `"auth"`) {
		t.Fatalf("lines = %q", lines)
	}
	var frame struct {
		Type    string `json:"type"`
		Message struct{ Role, Content string }
	}
	json.Unmarshal([]byte(lines[1]), &frame)
	if frame.Type != "user" || frame.Message.Content != "hello" {
		t.Fatalf("frame = %+v", frame)
	}
}

func TestChannelOrder(t *testing.T) {
	a := New()
	s := adapter.Session{Inbox: "/tmp/x.sock"}
	if a.Channel(s, adapter.State{MCPPush: true, HookSeen: true}) != adapter.ChannelMCP {
		t.Fatal("MCP push not preferred")
	}
	if a.Channel(s, adapter.State{HookSeen: true}) != adapter.ChannelCommand {
		t.Fatal("inbox post must beat hooks so idle sessions wake")
	}
	if a.Channel(adapter.Session{}, adapter.State{HookSeen: true}) != adapter.ChannelHook {
		t.Fatal("hooks without inbox")
	}
	if a.Channel(adapter.Session{}, adapter.State{}) != adapter.ChannelPull {
		t.Fatal("nothing known should mean pull")
	}
}

func TestPushUsesTheSessionsOwnToken(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "daemon-token-must-not-leak")
	sock := sockPath(t)
	l, _ := net.Listen("unix", sock)
	got := make(chan string, 1)
	go func() {
		c, _ := l.Accept()
		defer c.Close()
		b := make([]byte, 4096)
		n, _ := c.Read(b)
		got <- string(b[:n])
	}()
	New().Push(t.Context(), adapter.Session{Inbox: sock, InboxToken: "own"}, "x")
	if s := <-got; strings.Contains(s, "daemon-token") || !strings.Contains(s, `"own"`) {
		t.Fatalf("wrote %q", s)
	}
}

func TestHookOutputs(t *testing.T) {
	a := New()
	if out, ok := a.HookOutput(adapter.EventStop, "m"); !ok || out != `{"decision":"block","reason":"m"}` {
		t.Fatalf("stop = %q", out)
	}
	out, _ := a.HookOutput(adapter.EventTool, "m")
	if !strings.Contains(out, `"hookEventName":"PostToolUse"`) || !strings.Contains(out, `"additionalContext":"m"`) {
		t.Fatalf("tool = %q", out)
	}
}

func TestSessionFromRegistry(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "123.json"), []byte(`{"sessionId":"abc","cwd":"/x"}`), 0o600)
	if id, ok := SessionFromRegistry(dir, 123); !ok || id != "abc" {
		t.Fatalf("got %q %v", id, ok)
	}
}

func TestHookOutputEscapesJSON(t *testing.T) {
	a := New()
	text := `hello "world"
newline`
	out, ok := a.HookOutput(adapter.EventStop, text)
	if !ok {
		t.Fatal("stop output should be ok")
	}
	// Verify it's valid JSON and round-trips the text
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result["reason"] != text {
		t.Fatalf("reason mismatch: got %q, want %q", result["reason"], text)
	}
}
