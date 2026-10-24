package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

func TestInboxLongPollWakesOnArrival(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	done := make(chan InboxResult)
	go func() {
		done <- call[InboxResult](t, d, &fakeConn{id: 1}, "inbox", InboxArgs{Key: "codex:a", WaitSecs: 5})
	}()
	time.Sleep(50 * time.Millisecond)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:b", To: "alex/a-codex", Body: "now", Wake: true})
	select {
	case r := <-done:
		if !strings.Contains(r.Text, "now") {
			t.Fatalf("got %q", r.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll not woken")
	}
}

func TestUnreadCounts(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:b", To: "alex/a-codex", Body: "1", Wake: true})
	u := call[UnreadResult](t, d, &fakeConn{id: 1}, "unread", InboxArgs{Key: "codex:a"})
	if u.Count != 1 || u.From[0] != "alex/b-codex" {
		t.Fatalf("unread = %+v", u)
	}
}

// TestDeliverToWaitingInboxReturnsDeliveredReceipt checks the deferred Task 5 branch: a send to a
// session whose "inbox" long poll is already waiting is reported delivered, not queued/pulled.
func TestDeliverToWaitingInboxReturnsDeliveredReceipt(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	done := make(chan InboxResult, 1)
	go func() {
		done <- call[InboxResult](t, d, &fakeConn{id: 1}, "inbox", InboxArgs{Key: "codex:a", WaitSecs: 5})
	}()
	time.Sleep(50 * time.Millisecond)

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:b", To: "alex/a-codex", Body: "now", Wake: true})
	if res.Receipts[0].State != message.StateDelivered || !strings.Contains(res.Receipts[0].Note, "delivered: the session is waiting for messages") {
		t.Fatalf("receipt = %+v", res.Receipts[0])
	}
	<-done
}

// TestInboxNoLostWakeupUnderConcurrentSend checks that subscribing happens before the pending()
// check: a message stored concurrently, right as the long poll is about to wait, must still wake
// it at once rather than being missed until the full wait_secs elapses.
func TestInboxNoLostWakeupUnderConcurrentSend(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	for i := 0; i < 200; i++ {
		done := make(chan InboxResult, 1)
		go func() {
			done <- call[InboxResult](t, d, &fakeConn{id: 1}, "inbox", InboxArgs{Key: "codex:a", WaitSecs: 5})
		}()
		// from "cli:terminal": a session sender would trip the loop guard well before 200 sends.
		call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/a-codex", Body: fmt.Sprintf("hi%d", i), Wake: true})
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatalf("iteration %d: inbox did not wake within 1s (lost wakeup)", i)
		}
	}
}

// TestDeliveredClearsInFlight checks that "delivered" removes ids from in-flight tracking, not just MarkRead,
// so a push-pushed id doesn't get stuck hidden for the rest of the 90s in-flight window.
func TestDeliveredClearsInFlight(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelMCP))
	claudeConn := &fakeConn{id: 1}
	register(t, d, claudeConn, "claude-code", "c1", "/src/api", true)
	res := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/api-claude", Body: "hi", Wake: true})
	id := res.IDs[0]

	d.mu.Lock()
	_, inFlight := d.inFlight[id]
	d.mu.Unlock()
	if !inFlight {
		t.Fatal("expected the pushed id to be tracked in-flight")
	}

	call[map[string]any](t, d, &fakeConn{id: 3}, "delivered", DeliveredArgs{Key: "claude-code:c1", IDs: []string{id}})

	d.mu.Lock()
	_, stillInFlight := d.inFlight[id]
	d.mu.Unlock()
	if stillInFlight {
		t.Fatal("delivered did not clear the id from in-flight tracking")
	}
}

// TestInboxCtxCancelReturns checks that a long poll returns once its connection's ctx is cancelled,
// rather than waiting out wait_secs.
func TestInboxCtxCancelReturns(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(InboxArgs{Key: "codex:a", WaitSecs: 5})
	done := make(chan struct{})
	go func() {
		d.Handle(ctx, &fakeConn{id: 1}, "inbox", raw)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("inbox did not return when ctx was cancelled")
	}
}

// TestInboxWaitSecsCappedAt600 checks the cap without actually waiting 600s: wait_secs beyond the
// cap must not change behavior for an immediate arrival.
func TestInboxWaitSecsCappedAt600(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	register(t, d, &fakeConn{id: 2}, "codex", "b", "/src/b", false)
	call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "codex:b", To: "alex/a-codex", Body: "hi", Wake: false})
	r := call[InboxResult](t, d, &fakeConn{id: 1}, "inbox", InboxArgs{Key: "codex:a", WaitSecs: 10000})
	if len(r.IDs) != 1 {
		t.Fatalf("expected the already-pending message returned at once, got %+v", r)
	}
}

// pauseStore blocks Pending() until unblock is closed, giving a test a window to inspect daemon
// state while an "inbox" call is paused mid-check.
type pauseStore struct {
	store.Store
	unblock chan struct{}
}

func (s *pauseStore) Pending(mailbox string) ([]message.Message, error) {
	<-s.unblock
	return s.Store.Pending(mailbox)
}

// TestZeroWaitInboxNeverRegistersAsWaiting checks that a zero-wait "inbox" call never subscribes
// to arrivals (d.waiting stays 0 throughout), so deliver() can't mistake it for a long poll and
// short-circuit a concurrent send to "delivered" when the session actually has a push channel.
func TestZeroWaitInboxNeverRegistersAsWaiting(t *testing.T) {
	unblock := make(chan struct{})
	st := &pauseStore{Store: storetest.SQLite(t), unblock: unblock}
	d := New(Options{Person: "alex", Machine: "mbp", Store: st, Adapters: adapter.NewRegistry(claudeFake(adapter.ChannelMCP)),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf})
	claudeConn := &fakeConn{id: 1}
	register(t, d, claudeConn, "claude-code", "c1", "/src/api", true)

	done := make(chan InboxResult, 1)
	go func() {
		done <- call[InboxResult](t, d, &fakeConn{id: 2}, "inbox", InboxArgs{Key: "claude-code:c1", WaitSecs: 0})
	}()
	time.Sleep(50 * time.Millisecond) // let the call reach (and pause inside) pending()

	d.mu.Lock()
	waiting := d.waiting["claude-code:c1"]
	d.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("a zero-wait inbox call registered as waiting: %d", waiting)
	}

	res := call[SendResult](t, d, &fakeConn{id: 3}, "send", SendArgs{From: "cli:terminal", To: "alex/api-claude", Body: "hi", Wake: true})
	if res.Receipts[0].State != message.StatePushed {
		t.Fatalf("expected the push channel's receipt, not a false 'waiting' short-circuit, got %+v", res.Receipts[0])
	}

	close(unblock)
	<-done
}
