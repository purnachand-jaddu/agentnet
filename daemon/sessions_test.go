package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func TestNamesAndCollisions(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	a := register(t, d, &fakeConn{id: 1}, "codex", "1", "/src/api", false)
	b := register(t, d, &fakeConn{id: 2}, "codex", "2", "/src/api", false)
	if a.Address != "alex/api-codex" || b.Address != "alex/api-codex-2" {
		t.Fatalf("got %q, %q", a.Address, b.Address)
	}
}

func TestReturningSessionGetsItsNameBack(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	c := &fakeConn{id: 1}
	register(t, d, c, "codex", "1", "/src/api", false)
	d.ConnClosed(c)
	register(t, d, &fakeConn{id: 2}, "codex", "2", "/src/api", false) // takes the next free name
	back := register(t, d, &fakeConn{id: 3}, "codex", "1", "/src/api", false)
	if back.Name != "api-codex" {
		t.Fatalf("returning session named %q", back.Name)
	}
}

func TestMCPAttachesToHookSessionByPID(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}})
	r := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	if r.Key != "claude-code:c1" || r.Name != "api-claude" {
		t.Fatalf("MCP made a second session: %+v", r)
	}
}

func TestOffFolderRefused(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	d.opts.FolderAllowed = func(string) bool { return false }
	raw, _ := json.Marshal(RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/private"}})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 1}, "register", raw); err == nil {
		t.Fatal("registered in a folder that is off")
	}
}
