package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func TestHookGetsWakeMailMidTurnAndAllAtStop(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook), claudeFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStart, SessionID: "x1", Cwd: "/src/web"})
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "claude-code:c1", To: "alex/web-codex", Body: "fyi", Wake: false})
	tool := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventTool, SessionID: "x1", Cwd: "/src/web"})
	if tool.Out != "" {
		t.Fatalf("FYI mail interrupted a turn: %q", tool.Out)
	}
	stop := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStop, SessionID: "x1", Cwd: "/src/web"})
	if !strings.HasPrefix(stop.Out, "HOOK:") || len(stop.IDs) != 1 {
		t.Fatalf("stop hook = %+v", stop)
	}
	call[map[string]any](t, d, &fakeConn{id: 1}, "delivered", DeliveredArgs{Key: "codex:x1", IDs: stop.IDs})
	again := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStop, SessionID: "x1", Cwd: "/src/web"})
	if again.Out != "" {
		t.Fatal("delivered mail shown twice")
	}
}

func TestHookInOffFolderPrintsNothing(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	d.opts.FolderAllowed = func(string) bool { return false }
	r := call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStart, SessionID: "x1", Cwd: "/private"})
	if r.Out != "" || r.Key != "" {
		t.Fatalf("off-folder hook = %+v", r)
	}
}

func TestTurnWatchHandsMailToCommandWhenTurnEnds(t *testing.T) {
	cx := &watchFake{fakeAdapter: codexFake(adapter.ChannelHook)}
	cx.running = true
	d := newTestDaemon(t, cx, claudeFake(adapter.ChannelPull))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x1", Cwd: "/src/web"})
	register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "claude-code:c1", To: "alex/web-codex", Body: "ping", Wake: true})
	cx.mu.Lock()
	cx.running, cx.channel = false, adapter.ChannelCommand
	cx.mu.Unlock()
	waitFor(t, func() bool { return len(cx.Pushed()) == 1 })
}

type watchFake struct{ *fakeAdapter }

func (w *watchFake) TurnRunning(adapter.Session) (bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running, true
}
func (w *watchFake) Channel(adapter.Session, adapter.State) adapter.Channel {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.channel
}

// TestHookSetsBusyOnPromptAndIdleOnStop checks the busy/idle status rule directly (used by "who").
func TestHookSetsBusyOnPromptAndIdleOnStop(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x1", Cwd: "/src/web"})
	d.mu.Lock()
	busy := d.live["codex:x1"].busy
	d.mu.Unlock()
	if !busy {
		t.Fatal("expected busy after a prompt event")
	}
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStop, SessionID: "x1", Cwd: "/src/web"})
	d.mu.Lock()
	busy = d.live["codex:x1"].busy
	d.mu.Unlock()
	if busy {
		t.Fatal("expected idle after a stop event")
	}
}

// TestHookEndEndsSessionWithoutMCP checks that an "end" hook drops a session that has no MCP connection.
func TestHookEndEndsSessionWithoutMCP(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventStart, SessionID: "x1", Cwd: "/src/web"})
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventEnd, SessionID: "x1", Cwd: "/src/web"})
	d.mu.Lock()
	_, live := d.live["codex:x1"]
	d.mu.Unlock()
	if live {
		t.Fatal("expected the session gone after an end hook with no MCP connection")
	}
}

// TestHookEndKeepsSessionWithMCP checks that an "end" hook leaves a session alone when an MCP connection is attached.
func TestHookEndKeepsSessionWithMCP(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelHook))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "claude-code", Event: adapter.EventEnd, SessionID: "c1", Cwd: "/src/api", PID: 4242})

	d.mu.Lock()
	_, live := d.live["claude-code:c1"]
	d.mu.Unlock()
	if !live {
		t.Fatal("expected the session to survive an end hook while its MCP connection is attached")
	}
}

// TestAttachPicksSoleBusyHookSeenInCwd checks the happy path: one busy hook-seen session in the given cwd.
func TestAttachPicksSoleBusyHookSeenInCwd(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x1", Cwd: "/src/web"})
	r := call[Registered](t, d, &fakeConn{id: 2}, "attach", AttachArgs{Agent: "codex", Cwd: "/src/web"})
	if r.Key != "codex:x1" {
		t.Fatalf("attach = %+v", r)
	}
	d.mu.Lock()
	mcpPush := d.live["codex:x1"].mcpPush
	d.mu.Unlock()
	if !mcpPush {
		t.Fatal("expected attach to mark the session as push-capable")
	}
}

// TestAttachFallsBackAnywhereThenErrorsOnAmbiguity checks the cwd-miss fallback and the no-unique-match error.
func TestAttachFallsBackAnywhereThenErrorsOnAmbiguity(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x1", Cwd: "/src/web"})
	r := call[Registered](t, d, &fakeConn{id: 2}, "attach", AttachArgs{Agent: "codex", Cwd: "/src/elsewhere"})
	if r.Key != "codex:x1" {
		t.Fatalf("expected the fallback-anywhere match, got %+v", r)
	}

	call[HookResult](t, d, &fakeConn{id: 3}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x2", Cwd: "/src/other"})
	raw, _ := json.Marshal(AttachArgs{Agent: "codex", Cwd: "/src/nomatch"})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 4}, "attach", raw); err == nil {
		t.Fatal("expected an error with two busy sessions and no cwd match")
	}
}

// TestWhoListsBusyAndMarksCaller checks "who"'s status and You fields.
func TestWhoListsBusyAndMarksCaller(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook), claudeFake(adapter.ChannelPull))
	call[HookResult](t, d, &fakeConn{id: 1}, "hook", HookArgs{Agent: "codex", Event: adapter.EventPrompt, SessionID: "x1", Cwd: "/src/web"})
	reg := register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)

	res := call[WhoResult](t, d, &fakeConn{id: 3}, "who", WhoArgs{Key: reg.Key})
	var sawBusy, sawYou bool
	for _, e := range res.Sessions {
		if e.Status == "busy" {
			sawBusy = true
		}
		if e.You {
			sawYou = true
			if e.Address != reg.Address {
				t.Fatalf("You entry address = %q, want %q", e.Address, reg.Address)
			}
		}
	}
	if !sawBusy || !sawYou {
		t.Fatalf("who = %+v", res.Sessions)
	}
}
