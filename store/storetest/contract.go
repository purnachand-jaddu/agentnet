package storetest

import (
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

func msg(id, to string) message.Message {
	return message.Message{ID: id, From: "alice/a", To: to, Body: "hi " + id, Wake: true, SentAt: time.Unix(1, 0).UTC()}
}

// must fails the test if err is non-nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Run checks the behaviour every Store must have.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	t.Run("mailbox is FIFO and dedupes", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"1", "2", "1"} {
			_, err := s.PutMessage(msg(id, "bob/x"), "x", false)
			must(t, err)
		}
		got, err := s.Pending("x")
		must(t, err)
		if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
			t.Fatalf("Pending = %+v", got)
		}
		ok, err := s.Has("2")
		must(t, err)
		if !ok {
			t.Fatal("Has(2) = false")
		}
	})
	t.Run("message by id round trip", func(t *testing.T) {
		s := open(t)
		want := msg("1", "bob/x")
		_, err := s.PutMessage(want, "x", false)
		must(t, err)
		got, mailbox, err := s.MessageByID("1")
		must(t, err)
		if got == nil || *got != want || mailbox != "x" {
			t.Fatalf("MessageByID = %+v, %q", got, mailbox)
		}
		got, mailbox, err = s.MessageByID("nope")
		if got != nil || mailbox != "" || err != nil {
			t.Fatalf("absent MessageByID = %+v, %q, %v", got, mailbox, err)
		}
	})
	t.Run("accept stores once per id and keeps its tombstone", func(t *testing.T) {
		s := open(t)
		at := time.Unix(1000, 0)
		m := msg("A1", "bob/x")
		accept := func(m message.Message, sender string) store.AcceptOutcome {
			t.Helper()
			out, err := s.Accept(m, "x", false, sender, at)
			must(t, err)
			return out
		}
		if out := accept(m, "peerA/alice/a"); out != store.Inserted {
			t.Fatalf("first Accept = %v", out)
		}
		sender, recipient, ok, err := s.Received("A1")
		must(t, err)
		if !ok || sender != "peerA/alice/a" || recipient != "bob/x" {
			t.Fatalf("Received = %q, %q, %v", sender, recipient, ok)
		}
		if out := accept(m, "peerA/alice/a"); out != store.Duplicate {
			t.Fatalf("same sender and recipient = %v, want Duplicate", out)
		}
		other := m
		other.To = "bob/y"
		if out := accept(other, "peerA/alice/a"); out != store.Conflict {
			t.Fatalf("other recipient = %v, want Conflict", out)
		}
		if out := accept(m, "peerB/carol/c"); out != store.Conflict {
			t.Fatalf("other sender = %v, want Conflict", out)
		}
		got, err := s.Pending("x")
		must(t, err)
		if len(got) != 1 || got[0].From != "alice/a" {
			t.Fatalf("Pending = %+v", got)
		}

		must(t, s.MarkRead([]string{"A1"}, at))
		must(t, s.Drop([]string{"A1"}))
		if _, _, ok, _ := s.Received("A1"); !ok {
			t.Fatal("Drop removed the tombstone")
		}
		if out := accept(m, "peerA/alice/a"); out != store.Duplicate {
			t.Fatalf("after Drop = %v, want Duplicate", out)
		}
		if m, _, _ := s.MessageByID("A1"); m != nil {
			t.Fatal("a dropped message was stored again")
		}

		_, err = s.PutMessage(msg("L1", "bob/x"), "x", false) // a local message: no tombstone
		must(t, err)
		if out := accept(msg("L1", "bob/x"), "peerA/alice/a"); out != store.Conflict {
			t.Fatalf("id of a local message = %v, want Conflict", out)
		}
		if _, _, ok, _ := s.Received("L1"); ok {
			t.Fatal("a conflicting Accept left a tombstone")
		}

		sender, recipient, ok, err = s.Received("nope")
		if sender != "" || recipient != "" || ok || err != nil {
			t.Fatalf("absent Received = %q, %q, %v, %v", sender, recipient, ok, err)
		}
	})
	t.Run("prune received drops only old tombstones", func(t *testing.T) {
		s := open(t)
		t0 := time.Unix(1000, 0)
		_, err := s.Accept(msg("old", "bob/x"), "x", false, "p", t0)
		must(t, err)
		_, err = s.Accept(msg("new", "bob/x"), "x", false, "p", t0.Add(time.Hour))
		must(t, err)
		n, err := s.PruneReceived(t0.Add(time.Minute))
		must(t, err)
		_, _, oldOK, _ := s.Received("old")
		_, _, newOK, _ := s.Received("new")
		if n != 1 || oldOK || !newOK {
			t.Fatalf("pruned %d, old kept %v, new kept %v", n, oldOK, newOK)
		}
	})
	t.Run("read messages leave pending", func(t *testing.T) {
		s := open(t)
		_, err := s.PutMessage(msg("1", "bob/x"), "x", false)
		must(t, err)
		must(t, s.MarkRead([]string{"1"}, time.Now()))
		got, err := s.Pending("x")
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("Pending after read = %+v", got)
		}
		ok, err := s.Has("1")
		must(t, err)
		if !ok {
			t.Fatal("Has(1) after MarkRead = false")
		}
	})
	t.Run("held stays out of pending until released", func(t *testing.T) {
		s := open(t)
		_, err := s.PutMessage(msg("1", "bob/x"), "x", true)
		must(t, err)
		_, err = s.PutMessage(msg("2", "bob/x"), "x", true)
		must(t, err)
		got, err := s.Pending("x")
		must(t, err)
		if len(got) != 0 {
			t.Fatal("held message pending")
		}
		h, err := s.Held()
		must(t, err)
		if len(h) != 2 || h[0].Message.ID != "1" || h[1].Message.ID != "2" || h[0].Mailbox != "x" {
			t.Fatalf("Held = %+v", h)
		}
		held, err := s.IsHeld("1")
		must(t, err)
		if !held {
			t.Fatal("IsHeld(1) = false before Release")
		}
		must(t, s.Release([]string{"1", "2"}))
		got, err = s.Pending("x")
		must(t, err)
		if len(got) != 2 {
			t.Fatal("released message not pending")
		}
		ok, err := s.Has("1")
		must(t, err)
		if !ok {
			t.Fatal("Has(1) after Release = false")
		}
		held, err = s.IsHeld("1")
		must(t, err)
		if held {
			t.Fatal("IsHeld(1) = true after Release")
		}
		held, err = s.IsHeld("nope")
		must(t, err)
		if held {
			t.Fatal("IsHeld of an absent id = true")
		}
	})
	t.Run("drop removes a held message from Held and Pending", func(t *testing.T) {
		s := open(t)
		_, err := s.PutMessage(msg("1", "bob/x"), "x", true)
		must(t, err)
		_, err = s.PutMessage(msg("2", "bob/x"), "x", false)
		must(t, err)
		must(t, s.Drop([]string{"1", "2"}))
		h, err := s.Held()
		must(t, err)
		if len(h) != 0 {
			t.Fatalf("Held after Drop = %+v", h)
		}
		got, err := s.Pending("x")
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("Pending after Drop = %+v", got)
		}
	})
	t.Run("rehome moves unread mail, leaving read mail behind", func(t *testing.T) {
		s := open(t)
		_, err := s.PutMessage(msg("1", "bob/a"), "a", false)
		must(t, err)
		_, err = s.PutMessage(msg("2", "bob/a"), "a", false)
		must(t, err)
		_, err = s.PutMessage(msg("3", "bob/a"), "a", false)
		must(t, err)
		must(t, s.MarkRead([]string{"3"}, time.Now()))

		must(t, s.Rehome("a", "b"))

		got, err := s.Pending("b")
		must(t, err)
		if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
			t.Fatalf("Pending(b) after Rehome = %+v", got)
		}
		got, err = s.Pending("a")
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("Pending(a) after Rehome = %+v", got)
		}
		ok, err := s.Has("3")
		must(t, err)
		if !ok {
			t.Fatal("Rehome dropped the read message")
		}
	})
	t.Run("outbox by peer and age", func(t *testing.T) {
		s := open(t)
		t0 := time.Unix(100, 0)
		must(t, s.OutboxAdd("peerA", msg("1", "a/x"), t0))
		must(t, s.OutboxAdd("peerA", msg("2", "a/x"), t0.Add(time.Minute)))
		got, err := s.OutboxFor("peerA", t0.Add(30*time.Second))
		must(t, err)
		if len(got) != 1 || got[0].ID != "1" {
			t.Fatalf("OutboxFor = %+v", got)
		}
		peers, err := s.OutboxPeers()
		must(t, err)
		if len(peers) != 1 || peers[0] != "peerA" {
			t.Fatalf("OutboxPeers = %v", peers)
		}
		must(t, s.OutboxDone("1"))
		got, err = s.OutboxFor("peerA", t0.Add(time.Hour))
		must(t, err)
		if len(got) != 1 {
			t.Fatalf("after done = %+v", got)
		}
		n, err := s.OutboxPrune(t0.Add(2 * time.Minute))
		must(t, err)
		if n != 1 {
			t.Fatalf("pruned %d", n)
		}
	})
	t.Run("sessions remembered and forgotten", func(t *testing.T) {
		s := open(t)
		old := time.Unix(100, 0)
		must(t, s.UpsertSession(store.SessionRecord{Name: "api-claude", Key: "claude-code:1", Agent: "claude-code", LastSeen: old}))
		must(t, s.UpsertSession(store.SessionRecord{Name: "terminal", Key: "cli:terminal", Agent: "cli", LastSeen: old, Kept: true}))
		r, err := s.SessionByKey("claude-code:1")
		must(t, err)
		if r == nil || r.Name != "api-claude" {
			t.Fatalf("SessionByKey = %+v", r)
		}
		must(t, s.UpsertSession(store.SessionRecord{Name: "api-claude-2", Key: "claude-code:1", Agent: "claude-code", LastSeen: old}))
		r, err = s.SessionByName("api-claude")
		must(t, err)
		if r != nil {
			t.Fatal("one key kept two names")
		}
		n, err := s.ForgetSessions(old.Add(time.Hour))
		must(t, err)
		if n != 1 {
			t.Fatalf("forgot %d, want 1 (kept mailbox survives)", n)
		}
		r, err = s.SessionByName("terminal")
		must(t, err)
		if r == nil {
			t.Fatal("kept mailbox forgotten")
		}
	})
	t.Run("kv round trip", func(t *testing.T) {
		s := open(t)
		must(t, s.Put("cert", "k1", []byte("v1")))
		v, err := s.Get("cert", "k1")
		must(t, err)
		if string(v) != "v1" {
			t.Fatalf("Get = %q", v)
		}
		v, err = s.Get("cert", "nope")
		if v != nil || err != nil {
			t.Fatalf("absent Get = %q, %v", v, err)
		}
		all, err := s.List("cert")
		must(t, err)
		must(t, s.Delete("cert", "k1"))
		v, err = s.Get("cert", "k1")
		must(t, err)
		if len(all) != 1 || v != nil {
			t.Fatal("List/Delete wrong")
		}
	})
	t.Run("list on empty namespace", func(t *testing.T) {
		s := open(t)
		all, err := s.List("empty")
		must(t, err)
		if all == nil || len(all) != 0 {
			t.Fatalf("List(empty) = %v", all)
		}
	})
}
