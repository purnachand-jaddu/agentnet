package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
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

	first := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/web-codex", Body: "first", Wake: true})
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		l := d.liveByName("web-codex")
		return l != nil && l.pushFailed
	})

	// A failed background push drops its id from in-flight at once (fix #2): the fallback
	// (hook/pull) must see it right away rather than waiting out the 90s in-flight window.
	got, err := d.pending("codex:x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != first.IDs[0] {
		t.Fatalf("expected the failed push's id visible in pending at once, got %+v", got)
	}

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/web-codex", Body: "second", Wake: true})
	if res.Receipts[0].State != message.StateQueued {
		t.Fatalf("expected the second send to see PushFailed and go queued, got %+v", res.Receipts)
	}
}

// TestChannelMCPFallsBackWhenPushFails checks fix #1: a failed conn.Push drops the id from
// in-flight and reports queued, not pushed.
func TestChannelMCPFallsBackWhenPushFails(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelMCP))
	claudeConn := &fakeConn{id: 1, failPush: true}
	register(t, d, claudeConn, "claude-code", "c1", "/src/api", true)

	res := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/api-claude", Body: "hi", Wake: true})
	if res.Receipts[0].State != message.StateQueued || !strings.Contains(res.Receipts[0].Note, "checks its inbox") {
		t.Fatalf("expected queued once conn.Push fails, got %+v", res.Receipts)
	}

	got, err := d.pending("claude-code:c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != res.IDs[0] {
		t.Fatalf("expected the id visible in pending right away, got %+v", got)
	}
}

// TestPutMessageFailureBecomesRefusedReceipt checks fix #3: a store failure for one recipient
// becomes that recipient's refused receipt, and doesn't stop the other recipients.
type failingStore struct {
	store.Store
	failMailbox string
}

func (s *failingStore) PutMessage(m message.Message, mailbox string, held bool) (bool, error) {
	if mailbox == s.failMailbox {
		return false, errors.New("disk full")
	}
	return s.Store.PutMessage(m, mailbox, held)
}

func TestPutMessageFailureBecomesRefusedReceipt(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	regB := register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	d.opts.Store = &failingStore{Store: d.opts.Store, failMailbox: regB.Key}

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/*", Body: "go", Wake: true})
	if len(res.IDs) != 1 {
		t.Fatalf("expected only the stored recipient's id, got %v", res.IDs)
	}
	var sawRefused, sawQueued bool
	for _, r := range res.Receipts {
		switch r.State {
		case message.StateRefused:
			sawRefused = true
			if !strings.Contains(r.Note, "could not store the message") {
				t.Fatalf("unexpected refused note: %q", r.Note)
			}
		case message.StateQueued:
			sawQueued = true
		}
	}
	if !sawRefused || !sawQueued {
		t.Fatalf("expected one refused and one queued, got %+v", res.Receipts)
	}
}

// TestTerminalGroupSendNoLiveTargets checks fix #4: a group send with nobody live is an error.
func TestTerminalGroupSendNoLiveTargets(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	raw, _ := json.Marshal(SendArgs{From: "cli:terminal", To: "alex/*", Body: "hello", Wake: true})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 1}, "send", raw); err == nil {
		t.Fatal("expected an error when there are no live sessions to message")
	}
}

// TestResolveToRefusesOtherMachine checks fix #5: an address naming a different machine is refused.
func TestResolveToRefusesOtherMachine(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)
	raw, _ := json.Marshal(SendArgs{From: "cli:terminal", To: "alex/web-codex@otherhost", Body: "hi", Wake: true})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 2}, "send", raw); err == nil {
		t.Fatal("expected an error addressing another machine")
	}
}

// TestReverseJoinCarriesPlaceholderMail checks that mail sent to an MCP-only proc: placeholder
// isn't orphaned in its mailbox once a hook registration reverse-joins it to the real session key.
func TestReverseJoinCarriesPlaceholderMail(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register",
		RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/api-claude", Body: "hi", Wake: true})

	r := call[Registered](t, d, &fakeConn{id: 3}, "register",
		RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})

	got, err := d.pending(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "hi" {
		t.Fatalf("expected the placeholder's mail carried over to %s, got %+v", r.Key, got)
	}
}

// TestForgottenNameDoesNotLeakMail checks fix (a): mailboxes are keyed by session key, not
// name, so mail left for a forgotten session doesn't leak to a new session that later claims
// the same freed name.
func TestForgottenNameDoesNotLeakMail(t *testing.T) {
	now := time.Now()
	d := New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(codexFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: func() time.Time { return now }, Log: t.Logf})

	c1 := &fakeConn{id: 1}
	regOld := register(t, d, c1, "codex", "old", "/src/web", false)
	register(t, d, &fakeConn{id: 2}, "codex", "x2", "/src/api", false)
	d.ConnClosed(c1) // "old" goes offline; its remembered record (key, name) stays in the store

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:x2", To: "alex/" + regOld.Name, Body: "stale", Wake: true})
	if res.Receipts[0].State != message.StateQueued {
		t.Fatalf("expected queued for the offline session, got %+v", res.Receipts)
	}

	if _, err := d.opts.Store.ForgetSessions(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	regNew := register(t, d, &fakeConn{id: 4}, "codex", "new", "/src/web", false)
	if regNew.Name != regOld.Name {
		t.Fatalf("expected the new session to reuse the freed name, got %q vs %q", regNew.Name, regOld.Name)
	}

	got, err := d.pending(regNew.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no leaked mail for the new session, got %+v", got)
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

// Removing a live session's folder stops pushes to it, local or remote, without waiting for it to re-register.
func TestFolderRemovalStopsDeliveryToLiveSession(t *testing.T) {
	cx := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, cx)
	var removed atomic.Bool
	d.opts.FolderAllowed = func(dir string) bool { return !(removed.Load() && dir == "/src/web") }
	register(t, d, &fakeConn{id: 1}, "codex", "x1", "/src/web", false)
	removed.Store(true)

	res := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/web-codex", Body: "ping", Wake: true})
	if r := res.Receipts[0]; r.State == message.StatePushed {
		t.Fatalf("receipt %+v: pushed to a session whose folder was removed", r)
	}
	time.Sleep(50 * time.Millisecond)
	if p := cx.Pushed(); len(p) != 0 {
		t.Fatalf("pushed %q", p)
	}

	m := newMsg("sam/terminal", "alex/web-codex")
	if st, note := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); st != message.StateRefused || note != noSessionNote {
		t.Fatalf("remote admitMsg = %s/%q, want refused/%q", st, note, noSessionNote)
	}
	if ok, _ := d.opts.Store.Has(m.ID); ok {
		t.Fatal("remote message stored for a session whose folder was removed")
	}
}
