package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
	"github.com/purnachand-jaddu/agentnet/team"
)

// newMsg is a message from one address to another with a fresh id.
func newMsg(from, to string) message.Message {
	return message.Message{ID: message.NewID(), From: from, To: to, Body: "hi from " + from, Wake: true, SentAt: time.Now()}
}

// sealMsg builds the "msg" envelope carrying m from keys/cert to cert to.
func sealMsg(t *testing.T, keys identity.Keys, cert, to identity.Cert, m message.Message) envelope.Envelope {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	e, err := envelope.Build("msg", m.ID, keys, cert, to, payload, seal.HPKE{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// handle gives e to B directly and checks the answer is B's signed ack for e.
func handle(t *testing.T, f teamFixture, b *remoteB, e envelope.Envelope) envelope.Ack {
	t.Helper()
	ack := b.d.handleEnvelope(context.Background(), e)
	if err := envelope.VerifyAck(ack, f.bTeam.Self, e.ID); err != nil {
		t.Fatalf("ack %+v: %v", ack, err)
	}
	return ack
}

// fromA seals m from member A to B and hands it to B.
func fromA(t *testing.T, f teamFixture, b *remoteB, m message.Message) envelope.Ack {
	t.Helper()
	return handle(t, f, b, sealMsg(t, f.aKeys, f.aTeam.Self, f.bTeam.Self, m))
}

func wantAck(t *testing.T, ack envelope.Ack, st message.State, note string) {
	t.Helper()
	if ack.State != st || ack.Note != note {
		t.Fatalf("ack %s/%q, want %s/%q", ack.State, ack.Note, st, note)
	}
}

func storedMsg(t *testing.T, s store.Store, id string) *message.Message {
	t.Helper()
	m, _, err := s.MessageByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReceiveDeliversOverLAN(t *testing.T) {
	f := newTeamFixture(t)
	a, b, _ := startRemotePair(t, f)
	r := sendOne(t, a, "cli:terminal", "sam/web-codex").Receipts[0]
	if r.State != message.StatePushed {
		t.Fatalf("receipt %+v, want pushed (B's deliver state)", r)
	}
	waitUntil(t, 2*time.Second, "B's session gets it", func() bool { return len(b.fake.Pushed()) == 1 })
	if p := b.fake.Pushed()[0]; !strings.Contains(p, "hello sam/web-codex") {
		t.Fatalf("B's session got %q", p)
	}
	if n := outboxLen(t, f.aStore, peerKey(f.bKeys.Public())); n != 0 {
		t.Fatalf("A's outbox holds %d", n)
	}
}

func TestReceiveRetryAfterLostAckIsReAcked(t *testing.T) {
	f := newTeamFixture(t)
	a, b, _ := startRemotePair(t, f)
	bk := peerKey(f.bKeys.Public())
	m := newMsg("alex/terminal", "sam/web-codex")
	want := []struct {
		st   message.State
		note string
	}{{message.StatePushed, ""}, {message.StateDelivered, "already received"}}
	for i, w := range want {
		if err := f.aStore.OutboxAdd(bk, m, time.Now()); err != nil { // the second time: A never saw the first ack
			t.Fatal(err)
		}
		mu := a.peerLock(bk)
		mu.Lock()
		st, note, done := a.attempt(context.Background(), f.bTeam.Self, m)
		mu.Unlock()
		if st != w.st || note != w.note || !done {
			t.Fatalf("attempt %d: %s/%q done=%v, want %s/%q done", i, st, note, done, w.st, w.note)
		}
	}
	if n := outboxLen(t, f.aStore, bk); n != 0 {
		t.Fatalf("A's outbox holds %d", n)
	}
	waitUntil(t, 2*time.Second, "B's session gets it", func() bool { return len(b.fake.Pushed()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(b.fake.Pushed()); n != 1 {
		t.Fatalf("B's session got it %d times, want 1", n)
	}
}

func TestReceiveRefusesForgedSender(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	m := newMsg("sam/web-codex", "sam/web-codex") // A claiming to be one of B's person's sessions
	wantAck(t, fromA(t, f, b, m), message.StateRefused, "malformed")
	if storedMsg(t, f.bStore, m.ID) != nil {
		t.Fatal("forged message stored")
	}
}

func TestReceiveRefusesMismatchedID(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	m := newMsg("alex/terminal", "sam/web-codex")
	payload, _ := json.Marshal(m)
	e, err := envelope.Build("msg", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantAck(t, handle(t, f, b, e), message.StateRefused, "malformed")
}

func TestReceiveRefusesOtherFounderBeforeDecryption(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	_, epriv, _ := ed25519.GenerateKey(rand.Reader)
	eKeys, _ := identity.GenerateKeys()
	eTeam, err := team.Create(storetest.SQLite(t), eKeys, epriv, "eve", "evil", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := sealMsg(t, eKeys, eTeam.Self, f.bTeam.Self, newMsg("eve/terminal", "sam/web-codex"))
	e.Sealed = []byte("not even ciphertext") // a cert check after decryption would answer "malformed"
	wantAck(t, handle(t, f, b, e), message.StateRefused, "not a member")
}

func TestReceiveSameIDFromAnotherSenderRefused(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	cKeys, _ := identity.GenerateKeys()
	c, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(cKeys, "pat", "tp"), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.bTeam.Merge([]identity.Cert{c.Cert}, nil, time.Now())

	orig := newMsg("alex/terminal", "sam/web-codex")
	wantAck(t, fromA(t, f, b, orig), message.StatePushed, "")
	steal := orig
	steal.From, steal.Body = "pat/terminal", "pat's version"
	wantAck(t, handle(t, f, b, sealMsg(t, cKeys, c.Cert, f.bTeam.Self, steal)), message.StateRefused, "malformed")
	if got := storedMsg(t, f.bStore, orig.ID); got == nil || got.From != orig.From || got.Body != orig.Body {
		t.Fatalf("stored %+v, want A's original", got)
	}
	wantAck(t, fromA(t, f, b, orig), message.StateDelivered, "already received")
}

func TestReceiveRateLimitPerMachine(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	clk := &fakeClock{now: time.Now()}
	b.d.opts.Now = clk.Now
	for i := range rateMax {
		if ack := fromA(t, f, b, newMsg("alex/terminal", "sam/web-codex")); ack.State == message.StateRetry {
			t.Fatalf("message %d: ack %+v", i+1, ack)
		}
	}
	over := newMsg("alex/terminal", "sam/web-codex")
	if ack := fromA(t, f, b, over); ack.State != message.StateRetry {
		t.Fatalf("message %d: ack %+v, want retry", rateMax+1, ack)
	}
	if storedMsg(t, f.bStore, over.ID) != nil {
		t.Fatal("rate-limited message stored")
	}
	clk.Set(clk.Now().Add(rateWindow + time.Second))
	if ack := fromA(t, f, b, over); ack.State != message.StatePushed {
		t.Fatalf("after the window: ack %+v, want pushed", ack)
	}
}

func TestReceivePaused(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	b.paused.Store(true)
	m := newMsg("alex/terminal", "sam/web-codex")
	wantAck(t, fromA(t, f, b, m), message.StateRefused, "paused")
	b.paused.Store(false)
	wantAck(t, fromA(t, f, b, newMsg("alex/terminal", "sam/web-codex")), message.StatePushed, "")
}

func TestReceiveUnknownSession(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	for _, to := range []string{"sam/nope", "pat/web-codex", "sam/web-codex@other"} {
		wantAck(t, fromA(t, f, b, newMsg("alex/terminal", to)), message.StateRefused, "no such session")
	}
	// A remembered session that isn't live gets it when it's back.
	if err := f.bStore.UpsertSession(store.SessionRecord{Name: "old-claude", Key: "claude-code:9", Agent: "claude-code", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"sam/old-claude", "sam/old-claude@xps"} {
		wantAck(t, fromA(t, f, b, newMsg("alex/terminal", to)), message.StateQueued, "it reaches that session when it's back")
	}
}

func TestReceiveGroupOnlyFromTerminal(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	wantAck(t, fromA(t, f, b, newMsg("alex/web-codex", "sam/*")), message.StateRefused, "only the terminal can message a group")
}

func TestReceiveLoopGuard(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	for i := range loopMax + 1 {
		ack := fromA(t, f, b, newMsg("alex/web-codex", "sam/web-codex"))
		if refused := ack.State == message.StateRefused; refused != (i == loopMax) {
			t.Fatalf("message %d: ack %+v", i+1, ack)
		}
	}
	wantAck(t, fromA(t, f, b, newMsg("alex/terminal", "sam/web-codex")), message.StatePushed, "") // the terminal is exempt
}
