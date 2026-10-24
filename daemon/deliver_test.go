package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

func TestMCPPushDeliversAsEvent(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelMCP), codexFake(adapter.ChannelCommand))
	claudeConn := &fakeConn{id: 1}
	register(t, d, claudeConn, "claude-code", "c1", "/src/api", true)
	register(t, d, &fakeConn{id: 2}, "codex", "x1", "/src/web", false)
	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:x1", To: "alex/api-claude", Body: "hello", Wake: true})
	ev := claudeConn.Events()
	if res.Receipts[0].State != "pushed" || len(ev) != 1 || !strings.Contains(ev[0].Text, "hello") {
		t.Fatalf("receipt %+v, events %+v", res.Receipts, ev)
	}
}

func TestCommandPushRunsAdapter(t *testing.T) {
	cx := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, cx, claudeFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)
	register(t, d, &fakeConn{id: 2}, "claude-code", "c1", "/src/api", false)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "claude-code:c1", To: "alex/web-codex", Body: "ping", Wake: true})
	waitFor(t, func() bool { return len(cx.Pushed()) == 1 })
}

func TestAgentsCannotMessageGroups(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)
	raw, _ := json.Marshal(SendArgs{From: "codex:x1", To: "alex/*", Body: "all", Wake: true})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 2}, "send", raw); err == nil {
		t.Fatal("agent group send accepted")
	}
}

func TestTerminalGroupSendStoresOneCopyPerRecipient(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)
	register(t, d, &fakeConn{id: 2}, "codex", "x2", "/src/api", false)
	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/*", Body: "deploying", Wake: true})
	if len(res.IDs) != 2 || res.IDs[0] == res.IDs[1] {
		t.Fatalf("ids = %v", res.IDs)
	}
}

func TestLoopGuard(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	var last SendResult
	for i := 0; i < 13; i++ {
		last = call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:a", To: "alex/b-codex", Body: "x", Wake: true})
	}
	if last.Receipts[0].State != "refused" {
		t.Fatalf("13th message = %+v", last.Receipts)
	}
}

func TestOfflineMailWaits(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	c := &fakeConn{id: 1}
	register(t, d, c, "codex", "x1", "/src/web", false)
	register(t, d, &fakeConn{id: 2}, "codex", "x2", "/src/api", false)
	d.ConnClosed(c)
	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:x2", To: "alex/web-codex", Body: "later", Wake: true})
	if res.Receipts[0].State != "queued" || !strings.Contains(res.Receipts[0].Note, "when it's back") {
		t.Fatalf("receipt %+v", res.Receipts)
	}
}

// TestInFlightHiddenThenReappears checks the 90s window: an in-flight id is hidden from pending, then reappears.
func TestInFlightHiddenThenReappears(t *testing.T) {
	now := time.Now()
	d := New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(),
		FolderAllowed: func(string) bool { return true }, Now: func() time.Time { return now }, Log: t.Logf})

	m := message.Message{ID: message.NewID(), From: "alex/terminal", To: "alex/x", Body: "hi", Wake: true, SentAt: now}
	if _, err := d.opts.Store.PutMessage(m, "x", false); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.inFlight[m.ID] = now
	d.mu.Unlock()

	got, err := d.pending("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the in-flight id hidden, got %+v", got)
	}

	now = now.Add(91 * time.Second)
	got, err = d.pending("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("expected the id to reappear after 90s, got %+v", got)
	}
}

// TestCommandPushFailureSetsPushFailed checks that a failed ChannelCommand push marks the session so the
// next Channel() call (here, our fakeAdapter switching to ChannelHook) sees PushFailed.
func TestCommandPushFailureSetsPushFailed(t *testing.T) {
	cx := codexFake(adapter.ChannelCommand)
	cx.failPush = true
	d := newTestDaemon(t, cx)
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)

	call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/web-codex", Body: "first", Wake: true})
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		l := d.liveByName("web-codex")
		return l != nil && l.pushFailed
	})

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/web-codex", Body: "second", Wake: true})
	if res.Receipts[0].State != message.StateQueued {
		t.Fatalf("expected the second send to see PushFailed and go queued, got %+v", res.Receipts)
	}
}

// TestPushFailedClearsMCPPush checks that the "push_failed" op clears the session's mcpPush flag.
func TestPushFailedClearsMCPPush(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelMCP))
	register(t, d, &fakeConn{id: 1}, "claude-code", "c1", "/src/api", true)

	d.mu.Lock()
	mcpPush := d.liveByName("api-claude").mcpPush
	d.mu.Unlock()
	if !mcpPush {
		t.Fatal("expected mcpPush true right after register")
	}

	call[any](t, d, &fakeConn{id: 2}, "push_failed", PushFailedArgs{Key: "claude-code:c1", IDs: []string{"x"}})

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.liveByName("api-claude").mcpPush {
		t.Fatal("expected mcpPush false after push_failed")
	}
}
