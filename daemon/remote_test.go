package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
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

// retryBody makes B's hook answer retry for that message alone.
const retryBody = "please retry"

// startB runs B on port with session web-codex, its hook delivering locally while state is pushed.
func startB(t *testing.T, f teamFixture, port, aPort int) *remoteB {
	r := &remoteB{fake: codexFake(adapter.ChannelCommand)}
	r.setState(message.StatePushed)
	r.d = newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, port, lanAddr(aPort))
	r.d.opts.Adapters = adapter.NewRegistry(r.fake)
	r.d.opts.testAcceptMsg = func(m message.Message) message.State {
		r.got.Add(1)
		if m.Body == retryBody {
			return message.StateRetry
		}
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
	res, err := d.send(context.Background(), SendArgs{From: from, To: to, Body: "hello " + to, Wake: true})
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

	_, err = a.send(context.Background(), SendArgs{From: "cli:terminal", To: "sam/web-codex", Body: "x"})
	if err == nil || err.Error() != "ambiguous: name one of sam/web-codex@xps, sam/web-codex@xps2" {
		t.Fatalf("ambiguous: err = %v", err)
	}
	if _, err := a.send(context.Background(), SendArgs{From: "cli:terminal", To: "sam/nope", Body: "x"}); err == nil || err.Error() != "no such session in the network" {
		t.Fatalf("unknown: err = %v", err)
	}
	if _, err := a.send(context.Background(), SendArgs{From: "codex:a1", To: "sam/*", Body: "x"}); err == nil {
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

	res, err := a.send(context.Background(), SendArgs{From: "cli:terminal", To: "sam/*", Body: "x"})
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
	if ids := map[string]bool{}; len(res.IDs) != 3 || func() bool {
		for _, id := range res.IDs {
			ids[id] = true
		}
		return len(ids) != 3
	}() {
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

// idleA is A, not running (so only explicit ticks flush), knowing B at addr.
func idleA(t *testing.T, f teamFixture, addr string) *Daemon {
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, 0)
	a.mu.Lock()
	a.peers[peerKey(f.bKeys.Public())] = &peer{cert: f.bTeam.Self, verified: []string{addr}}
	a.mu.Unlock()
	return a
}

func queue(t *testing.T, s store.Store, peer, body string, at time.Time) message.Message {
	t.Helper()
	m := message.Message{ID: message.NewID(), From: "alex/terminal", To: "sam/web-codex", Body: body, Wake: true, SentAt: at}
	if err := s.OutboxAdd(peer, m, at); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFlushContinuesPastARetryAck(t *testing.T) {
	f := newTeamFixture(t)
	aPort, bPort := freePort(t), freePort(t)
	b := startB(t, f, bPort, aPort)
	a := idleA(t, f, lanAddr(bPort))
	bk := peerKey(f.bKeys.Public())
	now := time.Now()
	first := queue(t, f.aStore, bk, retryBody, now.Add(-2*time.Minute))
	queue(t, f.aStore, bk, "second", now.Add(-time.Minute))

	a.outboxTick(context.Background())
	waitUntil(t, 3*time.Second, "B is offered both", func() bool { return b.got.Load() == 2 })
	waitUntil(t, 2*time.Second, "only the retried one stays queued", func() bool {
		msgs, _ := f.aStore.OutboxFor(bk, now.Add(time.Hour))
		return len(msgs) == 1 && msgs[0].ID == first.ID
	})
}

func TestPresenceDuringFailedFlushFlushesAgain(t *testing.T) {
	f := newTeamFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	t.Cleanup(stale.Close)
	aPort, bPort := freePort(t), freePort(t)
	b := startB(t, f, bPort, aPort)
	a := idleA(t, f, "lan://"+strings.TrimPrefix(stale.URL, "http://"))
	bk := peerKey(f.bKeys.Public())
	queue(t, f.aStore, bk, "queued", time.Now().Add(-time.Minute))

	a.outboxTick(context.Background())
	<-entered // the flush is stuck on B's old address
	now := time.Now()
	payload, _ := json.Marshal(presence{Addrs: []string{lanAddr(bPort)}, TS: now.UnixMilli()})
	env, err := envelope.Build("presence", message.NewID(), f.bKeys, f.bTeam.Self, f.aTeam.Self, payload, seal.HPKE{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ack := a.handleEnvelope(context.Background(), env); ack.State != message.StateDelivered {
		t.Fatalf("presence ack %+v", ack)
	}
	close(release) // the in-flight attempt fails
	waitUntil(t, 5*time.Second, "B receives it via the new address", func() bool { return b.got.Load() == 1 })
	waitUntil(t, 2*time.Second, "A's outbox empties", func() bool { return outboxLen(t, f.aStore, bk) == 0 })
}

func TestOwnOtherMachineRoutesRemotely(t *testing.T) {
	f := newTeamFixture(t)
	k2, _ := identity.GenerateKeys()
	mine, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(k2, "alex", "laptop"), true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.aTeam.Merge([]identity.Cert{mine.Cert}, nil, time.Now())
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, 0)
	injectPeer(a, mine.Cert, "web-codex")
	for _, to := range []string{"alex/web-codex@laptop", "web-codex@laptop"} {
		r := sendOne(t, a, "cli:terminal", to).Receipts[0]
		if r.To != "alex/web-codex@laptop" || r.State != message.StateQueued {
			t.Fatalf("%s: receipt %+v, want queued to alex/web-codex@laptop", to, r)
		}
	}
	if n := outboxLen(t, f.aStore, peerKey(k2.Public())); n != 2 {
		t.Fatalf("laptop outbox holds %d, want 2", n)
	}
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
