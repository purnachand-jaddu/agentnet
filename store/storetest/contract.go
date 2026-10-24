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

// Run checks the behaviour every Store must have.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	t.Run("mailbox is FIFO and dedupes", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"1", "2", "1"} {
			if _, err := s.PutMessage(msg(id, "bob/x"), "x", false); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := s.Pending("x")
		if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
			t.Fatalf("Pending = %+v", got)
		}
		if ok, _ := s.Has("2"); !ok {
			t.Fatal("Has(2) = false")
		}
	})
	t.Run("read messages leave pending", func(t *testing.T) {
		s := open(t)
		s.PutMessage(msg("1", "bob/x"), "x", false)
		s.MarkRead([]string{"1"}, time.Now())
		if got, _ := s.Pending("x"); len(got) != 0 {
			t.Fatalf("Pending after read = %+v", got)
		}
	})
	t.Run("held stays out of pending until released", func(t *testing.T) {
		s := open(t)
		s.PutMessage(msg("1", "bob/x"), "x", true)
		if got, _ := s.Pending("x"); len(got) != 0 {
			t.Fatal("held message pending")
		}
		h, _ := s.Held()
		if len(h) != 1 || h[0].Mailbox != "x" {
			t.Fatalf("Held = %+v", h)
		}
		s.Release([]string{"1"})
		if got, _ := s.Pending("x"); len(got) != 1 {
			t.Fatal("released message not pending")
		}
	})
	t.Run("outbox by peer and age", func(t *testing.T) {
		s := open(t)
		t0 := time.Unix(100, 0)
		s.OutboxAdd("peerA", msg("1", "a/x"), t0)
		s.OutboxAdd("peerA", msg("2", "a/x"), t0.Add(time.Minute))
		got, _ := s.OutboxFor("peerA", t0.Add(30*time.Second))
		if len(got) != 1 || got[0].ID != "1" {
			t.Fatalf("OutboxFor = %+v", got)
		}
		if peers, _ := s.OutboxPeers(); len(peers) != 1 || peers[0] != "peerA" {
			t.Fatalf("OutboxPeers = %v", peers)
		}
		s.OutboxDone("1")
		if got, _ := s.OutboxFor("peerA", t0.Add(time.Hour)); len(got) != 1 {
			t.Fatalf("after done = %+v", got)
		}
		if n, _ := s.OutboxPrune(t0.Add(2 * time.Minute)); n != 1 {
			t.Fatalf("pruned %d", n)
		}
	})
	t.Run("sessions remembered and forgotten", func(t *testing.T) {
		s := open(t)
		old := time.Unix(100, 0)
		s.UpsertSession(store.SessionRecord{Name: "api-claude", Key: "claude-code:1", Agent: "claude-code", LastSeen: old})
		s.UpsertSession(store.SessionRecord{Name: "terminal", Key: "cli:terminal", Agent: "cli", LastSeen: old, Kept: true})
		if r, _ := s.SessionByKey("claude-code:1"); r == nil || r.Name != "api-claude" {
			t.Fatalf("SessionByKey = %+v", r)
		}
		s.UpsertSession(store.SessionRecord{Name: "api-claude-2", Key: "claude-code:1", Agent: "claude-code", LastSeen: old})
		if r, _ := s.SessionByName("api-claude"); r != nil {
			t.Fatal("one key kept two names")
		}
		if n, _ := s.ForgetSessions(old.Add(time.Hour)); n != 1 {
			t.Fatalf("forgot %d, want 1 (kept mailbox survives)", n)
		}
		if r, _ := s.SessionByName("terminal"); r == nil {
			t.Fatal("kept mailbox forgotten")
		}
	})
	t.Run("kv round trip", func(t *testing.T) {
		s := open(t)
		s.Put("cert", "k1", []byte("v1"))
		if v, _ := s.Get("cert", "k1"); string(v) != "v1" {
			t.Fatalf("Get = %q", v)
		}
		if v, err := s.Get("cert", "nope"); v != nil || err != nil {
			t.Fatalf("absent Get = %q, %v", v, err)
		}
		all, _ := s.List("cert")
		s.Delete("cert", "k1")
		if v, _ := s.Get("cert", "k1"); len(all) != 1 || v != nil {
			t.Fatal("List/Delete wrong")
		}
	})
}
