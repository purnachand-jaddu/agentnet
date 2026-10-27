package daemon

import (
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func opencodeFake(ch adapter.Channel) *fakeAdapter {
	return &fakeAdapter{name: "opencode", short: "opencode", channel: ch, mux: true}
}

func opencodeHookFake() *fakeAdapter { return opencodeFake(adapter.ChannelHook) }

// TestHookPollReturnsMailAndClaimsIt checks that a poll hook returns everything pending (like stop)
// and claims ids in-flight, so the plugin's delivered follow-up marks them read.
func TestHookPollReturnsMailAndClaimsIt(t *testing.T) {
	d := newTestDaemon(t, opencodeHookFake(), claudeFake(adapter.ChannelPull))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventStart, SessionID: "s1", Cwd: "/src/web"})
	register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "claude-code:c1", To: "alex/web-opencode", Body: "fyi", Wake: false})

	poll := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventPoll, SessionID: "s1", Cwd: "/src/web"})
	if !strings.HasPrefix(poll.Out, "HOOK:") || len(poll.IDs) != 1 {
		t.Fatalf("poll hook = %+v", poll)
	}

	call[map[string]any](t, d, &fakeConn{id: 1}, "delivered", DeliveredArgs{Key: "opencode:s1", IDs: poll.IDs})
	again := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventPoll, SessionID: "s1", Cwd: "/src/web"})
	if again.Out != "" {
		t.Fatal("delivered mail shown twice")
	}
}

// TestHookStatusTogglesBusyAndLeavesMailPending checks that the status event updates "who"'s busy
// flag and returns nothing: mail stays pending until a poll prints it.
func TestHookStatusTogglesBusyAndLeavesMailPending(t *testing.T) {
	d := newTestDaemon(t, opencodeHookFake(), claudeFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)

	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventStatus, SessionID: "s1", Cwd: "/src/web", State: "busy"})
	d.mu.Lock()
	busy := d.live["opencode:s1"].busy
	d.mu.Unlock()
	if !busy {
		t.Fatal("expected busy after a busy status")
	}

	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "claude-code:c1", To: "alex/web-opencode", Body: "hi", Wake: true})
	r := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventStatus, SessionID: "s1", Cwd: "/src/web", State: "idle"})
	if r.Out != "" || len(r.IDs) != 0 {
		t.Fatalf("status hook should return no mail, got %+v", r)
	}
	d.mu.Lock()
	busy = d.live["opencode:s1"].busy
	d.mu.Unlock()
	if busy {
		t.Fatal("expected idle after an idle status")
	}

	msgs, err := d.pending("opencode:s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected the message still pending after status, got %+v", msgs)
	}
}

// TestMCPRegisterWithRealKeyDoesNotJoinHookSessionByPID pins that a named meta key wins over the
// PID join: OpenCode's one server PID is shared by every session, so the pid join would fold
// session B into A the first time B's MCP calls arrive after A's plugin talk.
func TestMCPRegisterWithRealKeyDoesNotJoinHookSessionByPID(t *testing.T) {
	d := newTestDaemon(t, opencodeHookFake())
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventStatus, SessionID: "a", Cwd: "/src/oc", PID: 77, State: "busy"})
	got := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "opencode:b", Agent: "opencode", ID: "b", Cwd: "/src/oc", PID: 77}, MCPPID: 77})
	if got.Key != "opencode:b" {
		t.Fatalf("pid join overrode the meta key: %q", got.Key)
	}
}

// TestHookPollKeepsBusy pins that a poll never reports idleness: the plugin polls idle sessions,
// but a status busy may land late (its subprocess is unordered), and a poll finishing after it
// must not overwrite busy.
func TestHookPollKeepsBusy(t *testing.T) {
	d := newTestDaemon(t, opencodeHookFake())
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventStatus, SessionID: "s1", Cwd: "/src/web", State: "busy"})
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventPoll, SessionID: "s1", Cwd: "/src/web"})
	d.mu.Lock()
	busy := d.live["opencode:s1"].busy
	d.mu.Unlock()
	if !busy {
		t.Fatal("a poll reset busy to idle")
	}
}

// TestHookEndDropsSessionWithConn pins that an opencode session registered over MCP leaves the
// live list on its session-end: the conn on it belongs to the shared MCP server, not to one
// session, so the conn==nil guard (right for claude) keeps a deleted session advertised forever.
func TestHookEndDropsSessionWithConn(t *testing.T) {
	d := newTestDaemon(t, opencodeHookFake())
	register(t, d, &fakeConn{id: 1}, "opencode", "s1", "/src/web", false)
	call[HookResult](t, d, &fakeConn{id: 2}, "hook", HookArgs{Agent: "opencode", Event: adapter.EventEnd, SessionID: "s1", Cwd: "/src/web"})
	d.mu.Lock()
	_, ok := d.live["opencode:s1"]
	d.mu.Unlock()
	if ok {
		t.Fatal("session-end left an MCP-registered opencode session live")
	}
}
