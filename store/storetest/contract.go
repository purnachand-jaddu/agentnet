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
