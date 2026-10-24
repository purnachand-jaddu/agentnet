package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
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

// failingNameStore fails every SessionByName call, to prove register surfaces the error instead of spinning forever.
type failingNameStore struct {
	store.Store
	err error
}

func (f *failingNameStore) SessionByName(string) (*store.SessionRecord, error) { return nil, f.err }

func TestRegisterPropagatesStoreError(t *testing.T) {
	st := &failingNameStore{Store: storetest.SQLite(t), err: errors.New("boom")}
	d := New(Options{Person: "alex", Machine: "mbp", Store: st, Adapters: adapter.NewRegistry(codexFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf})
	raw, _ := json.Marshal(RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api"}})

	done := make(chan error, 1)
	go func() {
		_, err := d.Handle(context.Background(), &fakeConn{id: 1}, "register", raw)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("register succeeded despite a failing store")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("register hung with a failing store (mutex deadlock)")
	}
}

func TestMCPAttachPicksMostRecentHookSession(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c2", Agent: "claude-code", ID: "c2", Cwd: "/src/api", PID: 4242}})

	r := call[Registered](t, d, &fakeConn{id: 3}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	if r.Key != "claude-code:c2" {
		t.Fatalf("MCP attached to %q, want the more recently hook-registered claude-code:c2", r.Key)
	}
}

func TestRegisterMergeKeepsExistingFields(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api", PID: 111, Inbox: "sock1"}})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api"}})

	d.mu.Lock()
	s := d.live["codex:1"].s
	d.mu.Unlock()
	if s.PID != 111 || s.Inbox != "sock1" {
		t.Fatalf("bare re-register wiped fields: %+v", s)
	}
}

func TestReverseJoinMCPThenHook(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	r := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}})

	if r.Key != "claude-code:c1" {
		t.Fatalf("got key %q, want claude-code:c1", r.Key)
	}
	if r.Name != "api-claude" {
		t.Fatalf("got name %q, want api-claude (the proc: placeholder should have freed it)", r.Name)
	}
	d.mu.Lock()
	l, stillLive := d.live["claude-code:c1"]
	_, stillProc := d.live["proc:999"]
	d.mu.Unlock()
	if !stillLive || !l.mcpPush {
		t.Fatalf("mcpPush not merged onto claude-code:c1: %+v", l)
	}
	if stillProc {
		t.Fatal("proc:999 placeholder was not removed after the reverse join")
	}
}

func TestFromClientExactMatch(t *testing.T) {
	claude := claudeFake(adapter.ChannelPull)
	codex := codexFake(adapter.ChannelPull)
	codex.clientNames = []string{"codex-mcp-client"}
	r := adapter.NewRegistry(claude, codex)

	if _, ok := r.FromClient("claude-ai"); ok {
		t.Fatal("claude-ai matched the claude-code fake")
	}
	got, ok := r.FromClient("codex-mcp-client")
	if !ok || got.Name() != "codex" {
		t.Fatalf("FromClient(codex-mcp-client) = %v, %v", got, ok)
	}
	got, ok = r.FromClient("claude-code")
	if !ok || got.Name() != "claude-code" {
		t.Fatalf("FromClient(claude-code) = %v, %v", got, ok)
	}
}

func TestSlugBase(t *testing.T) {
	cases := map[string]string{
		"/src/api":  "api",
		"/src/api-": "api",
		"/a  b":     "a-b",
		"/日本語":      "session",
		"/":         "session",
		"":          "session",
	}
	for cwd, want := range cases {
		if got := slugBase(cwd); got != want {
			t.Errorf("slugBase(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestBuildNameNoTrailingDashAfterTruncation(t *testing.T) {
	cases := []struct {
		base, short string
		n           int
		want        string
	}{
		{"aaaa-bbbb", strings.Repeat("s", 34), 1, "aaaa-" + strings.Repeat("s", 34)},
		{"aaaa-bbbb", strings.Repeat("s", 32), 2, "aaaa-" + strings.Repeat("s", 32) + "-2"},
	}
	for _, c := range cases {
		got := buildName(c.base, c.short, c.n)
		if got != c.want {
			t.Errorf("buildName(%q, %q, %d) = %q, want %q", c.base, c.short, c.n, got, c.want)
		}
		if len(got) > 40 {
			t.Errorf("buildName(%q, %q, %d) = %q, too long (%d)", c.base, c.short, c.n, got, len(got))
		}
	}
}
