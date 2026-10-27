package daemon

import (
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func opencodeFake(ch adapter.Channel) *fakeAdapter {
	return &fakeAdapter{name: "opencode", short: "opencode", channel: ch}
}

// TestHookPollReturnsMailAndClaimsIt checks that a poll hook returns everything pending (like stop)
// and claims ids in-flight, so the plugin's delivered follow-up marks them read.
func TestHookPollReturnsMailAndClaimsIt(t *testing.T) {
	d := newTestDaemon(t, opencodeFake(adapter.ChannelHook), claudeFake(adapter.ChannelPull))
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
	d := newTestDaemon(t, opencodeFake(adapter.ChannelHook), claudeFake(adapter.ChannelPull))
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
