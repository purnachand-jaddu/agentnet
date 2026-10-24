package daemon

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/team"
)

// remoteB is one run of member B with a command-channel adapter and the test-only msg hook.
type remoteB struct {
	d     *Daemon
	fake  *fakeAdapter
	got   atomic.Int32                  // msg envelopes that reached the hook
	state atomic.Pointer[message.State] // what the hook answers
	stop  func()
}

// startB runs B on port with session web-codex, its hook delivering locally while state is pushed.
func startB(t *testing.T, f teamFixture, port, aPort int) *remoteB {
	r := &remoteB{fake: codexFake(adapter.ChannelCommand)}
	r.setState(message.StatePushed)
	r.d = newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, port, lanAddr(aPort))
	r.d.opts.Adapters = adapter.NewRegistry(r.fake)
	r.d.opts.testAcceptMsg = func(m message.Message) message.State {
		r.got.Add(1)
		if st := *r.state.Load(); st != message.StatePushed {
			return st
		}
		addr, _ := message.ParseAddress(m.To)
		tgs, err := r.d.resolveTo("cli:terminal", addr.Session)
		if err != nil {
			return message.StateRefused
		}
		if _, err := r.d.opts.Store.PutMessage(m, tgs[0].key, false); err != nil {
			return message.StateRefused
		}
		return r.d.deliver(tgs[0].key, tgs[0].name, []message.Message{m}, m.Wake).State
	}
	register(t, r.d, &fakeConn{id: 1}, "codex", "b1", "/tmp/web", false)
	r.stop = runDaemon(t, r.d)
	return r
}

func (r *remoteB) setState(s message.State) { r.state.Store(&s) }

// startRemotePair runs A and B and waits until A lists B's web-codex.
func startRemotePair(t *testing.T, f teamFixture) (a *Daemon, b *remoteB, aPort int) {
	aPort, bPort := freePort(t), freePort(t)
	a = newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, aPort, lanAddr(bPort))
	b = startB(t, f, bPort, aPort)
	runDaemon(t, a)
	waitUntil(t, 5*time.Second, "A lists sam/web-codex", func() bool { return statusOf(a, "sam/web-codex") == "idle" })
	return a, b, aPort
}

func outboxLen(t *testing.T, s store.Store, peer string) int {
	t.Helper()
	msgs, err := s.OutboxFor(peer, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

func sendOne(t *testing.T, d *Daemon, from, to string) SendResult {
	t.Helper()
	res, err := d.send(SendArgs{From: from, To: to, Body: "hello " + to, Wake: true})
	if err != nil {
		t.Fatalf("send to %s: %v", to, err)
	}
	if len(res.Receipts) != 1 {
		t.Fatalf("receipts = %+v", res.Receipts)
	}
	return res
}

func TestRemoteSendQueuedThenFlushedOnPresence(t *testing.T) {
	f := newTeamFixture(t)
	a, b, aPort := startRemotePair(t, f)
	bk := peerKey(f.bKeys.Public())

	res := sendOne(t, a, "cli:terminal", "sam/web-codex")
	if r := res.Receipts[0]; r.State != message.StatePushed || r.To != "sam/web-codex" {
		t.Fatalf("receipt %+v, want pushed to sam/web-codex", r)
	}
	waitUntil(t, 2*time.Second, "B's adapter gets it", func() bool { return len(b.fake.Pushed()) == 1 })
	if n := outboxLen(t, f.aStore, bk); n != 0 {
		t.Fatalf("outbox holds %d after a pushed ack", n)
	}

	b.stop()
	res = sendOne(t, a, "cli:terminal", "sam/web-codex")
	if r := res.Receipts[0]; r.State != message.StateQueued || r.Note != unreachableNote {
		t.Fatalf("receipt %+v, want queued/unreachable", r)
	}
	if n := outboxLen(t, f.aStore, bk); n != 1 {
		t.Fatalf("outbox holds %d, want 1", n)
	}

	b2 := startB(t, f, freePort(t), aPort) // same store, new port; OutboxEvery is 30 s, so only presence can flush
	waitUntil(t, 5*time.Second, "B's restart receives the queued message", func() bool { return b2.got.Load() == 1 })
	waitUntil(t, 2*time.Second, "A's outbox empties", func() bool { return outboxLen(t, f.aStore, bk) == 0 })
	time.Sleep(1500 * time.Millisecond) // more presence rounds: any further flush would resend
	if n := b2.got.Load(); n != 1 {
		t.Fatalf("B received the queued message %d times, want 1", n)
	}
	if p := b2.fake.Pushed(); len(p) != 1 || !strings.Contains(p[0], "hello sam/web-codex") {
		t.Fatalf("B's adapter got %q", p)
	}
}

func TestRemoteRetryKeepsRefusedRemoves(t *testing.T) {
	f := newTeamFixture(t)
	a, b, _ := startRemotePair(t, f)
	bk := peerKey(f.bKeys.Public())

	b.setState(message.StateRetry)
	if r := sendOne(t, a, "cli:terminal", "sam/web-codex").Receipts[0]; r.State != message.StateRetry {
		t.Fatalf("receipt %+v, want retry", r)
	}
	if n := outboxLen(t, f.aStore, bk); n != 1 {
		t.Fatalf("outbox holds %d after a retry ack, want 1", n)
	}

	b.setState(message.StateRefused)
	if r := sendOne(t, a, "cli:terminal", "sam/web-codex").Receipts[0]; r.State != message.StateRefused {
		t.Fatalf("receipt %+v, want refused", r)
	}
	if n := outboxLen(t, f.aStore, bk); n != 1 {
		t.Fatalf("outbox holds %d after a refused ack, want only the retry one", n)
	}
	if n := b.got.Load(); n != 2 {
		t.Fatalf("hook saw %d messages, want 2", n)
	}
}

// injectPeer puts cert's machine in d's directory with sessions, as if its presence just arrived.
func injectPeer(d *Daemon, cert identity.Cert, sessions ...string) {
	var es []WhoEntry
	for _, s := range sessions {
		es = append(es, WhoEntry{Address: s, Agent: "codex", Status: "idle"})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peers[peerKey(cert.Key)] = &peer{cert: cert, sessions: es, lastSeen: d.opts.Now()}
}

func TestRemoteResolve(t *testing.T) {
	f := newTeamFixture(t)
	k2, _ := identity.GenerateKeys()
	b2, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(k2, "sam", "xps2"), true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.aTeam.Merge([]identity.Cert{b2.Cert}, nil, time.Now())
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, 0)
	injectPeer(a, f.bTeam.Self, "web-codex", "api-claude")
	injectPeer(a, b2.Cert, "web-codex")
	register(t, a, &fakeConn{id: 1}, "codex", "a1", "/tmp/alpha", false)

	_, err = a.send(SendArgs{From: "cli:terminal", To: "sam/web-codex", Body: "x"})
	if err == nil || err.Error() != "ambiguous: name one of sam/web-codex@xps, sam/web-codex@xps2" {
		t.Fatalf("ambiguous: err = %v", err)
	}
	if _, err := a.send(SendArgs{From: "cli:terminal", To: "sam/nope", Body: "x"}); err == nil || err.Error() != "no such session in the network" {
		t.Fatalf("unknown: err = %v", err)
	}
	if _, err := a.send(SendArgs{From: "codex:a1", To: "sam/*", Body: "x"}); err == nil {
		t.Fatal("group send from a session was allowed")
	}

	// No address is known for xps2, so it queues.
	r := sendOne(t, a, "cli:terminal", "sam/web-codex@xps2").Receipts[0]
	if r.To != "sam/web-codex@xps2" || r.State != message.StateQueued {
		t.Fatalf("receipt %+v", r)
	}
	if n := outboxLen(t, f.aStore, peerKey(k2.Public())); n != 1 {
		t.Fatalf("xps2 outbox holds %d, want 1", n)
	}

	res, err := a.send(SendArgs{From: "cli:terminal", To: "sam/*", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var tos []string
	for _, r := range res.Receipts {
		tos = append(tos, r.To)
	}
	slices.Sort(tos)
	if strings.Join(tos, ",") != "sam/api-claude,sam/web-codex@xps,sam/web-codex@xps2" {
		t.Fatalf("group receipts to %v", tos)
	}
	if len(res.IDs) != 3 || res.IDs[0] == res.IDs[1] {
		t.Fatalf("ids %v, want 3 distinct", res.IDs)
	}

	// The loop guard counts remote pairs too.
	for i := range loopMax + 1 {
		r := sendOne(t, a, "codex:a1", "sam/api-claude").Receipts[0]
		if refused := r.State == message.StateRefused; refused != (i == loopMax) {
			t.Fatalf("send %d: receipt %+v", i, r)
		}
	}
}

func TestOutboxTickResendsOnlyAgedEntries(t *testing.T) {
	f := newTeamFixture(t)
	aPort, bPort := freePort(t), freePort(t)
	b := startB(t, f, bPort, aPort)
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, aPort) // not running: no presence-triggered flush
	bk := peerKey(f.bKeys.Public())
	a.mu.Lock()
	a.peers[bk] = &peer{cert: f.bTeam.Self, verified: []string{lanAddr(bPort)}}
	a.mu.Unlock()

	now := time.Now()
	aged := message.Message{ID: message.NewID(), From: "alex/terminal", To: "sam/web-codex", Body: "aged", Wake: true, SentAt: now}
	fresh := message.Message{ID: message.NewID(), From: "alex/terminal", To: "sam/web-codex", Body: "fresh", Wake: true, SentAt: now}
	if err := f.aStore.OutboxAdd(bk, aged, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.aStore.OutboxAdd(bk, fresh, now); err != nil {
		t.Fatal(err)
	}
	a.outboxTick(context.Background())
	waitUntil(t, 3*time.Second, "B receives the aged entry", func() bool { return b.got.Load() == 1 })
	waitUntil(t, 2*time.Second, "only the fresh entry stays queued", func() bool {
		msgs, _ := f.aStore.OutboxFor(bk, now.Add(time.Hour))
		return len(msgs) == 1 && msgs[0].ID == fresh.ID
	})
}

func TestOutboxPrunesAfterAWeek(t *testing.T) {
	f := newTeamFixture(t)
	t0 := time.Now()
	clk := &fakeClock{now: t0}
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, 0)
	a.opts.Now = clk.Now
	old := message.Message{ID: message.NewID(), To: "sam/web-codex", Body: "old"}
	recent := message.Message{ID: message.NewID(), To: "sam/web-codex", Body: "recent"}
	if err := f.aStore.OutboxAdd("p", old, t0.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.aStore.OutboxAdd("p", recent, t0.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a.outboxTick(context.Background())
	msgs, err := f.aStore.OutboxFor("p", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != recent.ID {
		t.Fatalf("outbox after prune = %+v, want only the recent one", msgs)
	}
}
