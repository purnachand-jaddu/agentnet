package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/discovery/static"
	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
	"github.com/purnachand-jaddu/agentnet/team"
	"github.com/purnachand-jaddu/agentnet/transport"
	"github.com/purnachand-jaddu/agentnet/transport/lan"
)

// teamFixture is founder A (alex@mbp) plus admitted member B (sam@xps), each with its own store.
type teamFixture struct {
	fpriv          ed25519.PrivateKey
	aStore, bStore store.Store
	aTeam, bTeam   *team.State
	aKeys, bKeys   identity.Keys
}

func newTeamFixture(t *testing.T) teamFixture {
	t.Helper()
	now := time.Now()
	_, fpriv, _ := ed25519.GenerateKey(rand.Reader)
	f := teamFixture{fpriv: fpriv, aStore: storetest.SQLite(t), bStore: storetest.SQLite(t)}
	f.aKeys, _ = identity.GenerateKeys()
	f.bKeys, _ = identity.GenerateKeys()
	if _, err := team.Create(f.aStore, f.aKeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}
	bundle, err := team.Admit(f.aStore, fpriv, team.NewRequest(f.bKeys, "sam", "xps"), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if f.bTeam, err = team.Accept(f.bStore, f.bKeys, bundle, now); err != nil {
		t.Fatal(err)
	}
	if f.aTeam, err = team.Load(f.aStore); err != nil {
		t.Fatal(err)
	}
	return f
}

// freePort returns a currently unused 127.0.0.1 TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func lanAddr(port int) string { return fmt.Sprintf("lan://127.0.0.1:%d", port) }

// quietLog logs via t until the test ends, then drops lines, so late goroutines can't log after completion.
func quietLog(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() { mu.Lock(); done = true; mu.Unlock() })
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

func newNetDaemon(t *testing.T, person, machine string, st store.Store, tm *team.State, keys identity.Keys, port int, peers ...string) *Daemon {
	return New(Options{
		Person: person, Machine: machine, Store: st, Adapters: adapter.NewRegistry(codexFake(adapter.ChannelHook)),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: quietLog(t),
		Team: tm, Keys: keys, Sealer: seal.HPKE{},
		Transports:    []transport.Transport{lan.New("127.0.0.1", port)},
		Discoverers:   []discovery.Discoverer{static.New(peers)},
		PresenceEvery: time.Second, OfflineAfter: 3 * time.Second, Debounce: 50 * time.Millisecond,
	})
}

// runDaemon runs d until the returned stop is called (or the test ends), waiting for Run to return.
func runDaemon(t *testing.T, d *Daemon) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func waitUntil(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// statusOf returns addr's status in d's who, or "" if it isn't listed.
func statusOf(d *Daemon, addr string) string {
	for _, e := range d.who(WhoArgs{}).Sessions {
		if e.Address == addr {
			return e.Status
		}
	}
	return ""
}

// startPair runs A and B with static discovery pointing at each other, each with one local session.
func startPair(t *testing.T, f teamFixture) (a, b *Daemon, stopB func()) {
	pa, pb := freePort(t), freePort(t)
	a = newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, pa, lanAddr(pb))
	b = newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, pb, lanAddr(pa))
	register(t, a, &fakeConn{id: 1}, "codex", "a1", "/tmp/alpha", false)
	register(t, b, &fakeConn{id: 1}, "codex", "b1", "/tmp/beta", false)
	stopB = runDaemon(t, b)
	runDaemon(t, a)
	waitUntil(t, 5*time.Second, "each daemon lists the other's session", func() bool {
		return statusOf(a, "sam/beta-codex") == "idle" && statusOf(b, "alex/alpha-codex") == "idle"
	})
	return a, b, stopB
}

func TestPresenceShowsRemoteSessionsThenOffline(t *testing.T) {
	a, _, stopB := startPair(t, newTeamFixture(t))
	if statusOf(a, "alex/alpha-codex") != "idle" {
		t.Fatal("local entry changed")
	}
	stopB()
	waitUntil(t, 6*time.Second, "A shows B's session offline", func() bool {
		return statusOf(a, "sam/beta-codex") == "offline"
	})
}

func TestUnverifiedHintKeepsVerifiedAddress(t *testing.T) {
	f := newTeamFixture(t)
	a, b, _ := startPair(t, f)
	bk := peerKey(f.bKeys.Public())
	a.mu.Lock()
	before := slices.Clone(a.peers[bk].verified)
	a.mu.Unlock()
	if len(before) == 0 {
		t.Fatal("A has no verified address for B")
	}

	a.found(discovery.Found{Key: f.bKeys.Public(), Addrs: []string{"lan://127.0.0.1:1"}})
	a.mu.Lock()
	after := slices.Clone(a.peers[bk].verified)
	a.mu.Unlock()
	if !slices.Equal(before, after) {
		t.Fatalf("verified addresses changed by a hint: %v -> %v", before, after)
	}

	register(t, a, &fakeConn{id: 2}, "codex", "a2", "/tmp/gamma", false)
	waitUntil(t, 5*time.Second, "B sees A's new session", func() bool {
		return statusOf(b, "alex/gamma-codex") == "idle"
	})
}

func TestPresenceFromNonMemberRefused(t *testing.T) {
	f := newTeamFixture(t)
	now := time.Now()
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)

	// D is a real member B hasn't heard of yet; merging presence that carries D's cert would add it.
	dKeys, _ := identity.GenerateKeys()
	dBundle, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(dKeys, "pat", "tp"), false, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(presence{Certs: []identity.Cert{dBundle.Cert}, TS: now.UnixMilli()})

	// Another founder's machine: a validly signed envelope, but not from this team.
	_, epriv, _ := ed25519.GenerateKey(rand.Reader)
	eKeys, _ := identity.GenerateKeys()
	eTeam, err := team.Create(storetest.SQLite(t), eKeys, epriv, "eve", "evil", now)
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.Build("presence", message.NewID(), eKeys, eTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, now)
	if err != nil {
		t.Fatal(err)
	}
	ack := b.handleEnvelope(context.Background(), env)
	if ack.State != message.StateRefused || envelope.VerifyAck(ack, f.bTeam.Self, env.ID) != nil {
		t.Fatalf("non-member presence: ack %+v, want signed refused", ack)
	}
	if _, ok := f.bTeam.CertFor(dKeys.Public()); ok {
		t.Fatal("non-member's certs were merged")
	}

	// Control: the same payload from member A is accepted and merged.
	env, _ = envelope.Build("presence", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, now)
	if ack := b.handleEnvelope(context.Background(), env); ack.State != message.StateDelivered {
		t.Fatalf("member presence: ack %+v, want delivered", ack)
	}
	if _, ok := f.bTeam.CertFor(dKeys.Public()); !ok {
		t.Fatal("member's certs were not merged")
	}
	if reloaded, _ := team.Load(f.bStore); reloaded == nil {
		t.Fatal("team state lost after Persist")
	} else if _, ok := reloaded.CertFor(dKeys.Public()); !ok {
		t.Fatal("merged cert was not persisted")
	}

	// "msg" is not handled yet: a signed refusal.
	env, _ = envelope.Build("msg", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, []byte("{}"), seal.HPKE{}, now)
	if ack := b.handleEnvelope(context.Background(), env); ack.State != message.StateRefused || ack.Note != "not yet supported" {
		t.Fatalf("msg: ack %+v, want refused/not yet supported", ack)
	}
}

// countingTransport records Listen calls and does nothing else.
type countingTransport struct{ listens atomic.Int32 }

func (c *countingTransport) Schemes() []string { return []string{"fake"} }
func (c *countingTransport) Listen(context.Context, transport.Handler) ([]string, error) {
	c.listens.Add(1)
	return nil, nil
}
func (c *countingTransport) Send(context.Context, string, envelope.Envelope) (envelope.Ack, error) {
	return envelope.Ack{}, fmt.Errorf("fake")
}
func (c *countingTransport) Close() error { return nil }

func TestNotInNetworkNeverListens(t *testing.T) {
	f := newTeamFixture(t)
	for _, tc := range []struct {
		name string
		team *team.State
		want int32
	}{{"no team", nil, 0}, {"in team", f.aTeam, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			ft := &countingTransport{}
			d := New(Options{Person: "alex", Machine: "mbp", Store: f.aStore, Adapters: adapter.NewRegistry(),
				FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: quietLog(t),
				Team: tc.team, Keys: f.aKeys, Transports: []transport.Transport{ft}})
			stop := runDaemon(t, d)
			time.Sleep(100 * time.Millisecond)
			stop()
			if got := ft.listens.Load(); got != tc.want {
				t.Fatalf("Listen calls = %d, want %d", got, tc.want)
			}
		})
	}
}

// flakyStore fails Put while fail is set, standing in for a transient storage error.
type flakyStore struct {
	store.Store
	fail atomic.Bool
}

func (s *flakyStore) Put(ns, key string, val []byte) error {
	if s.fail.Load() {
		return fmt.Errorf("disk full")
	}
	return s.Store.Put(ns, key, val)
}

func TestFailedPersistIsRetried(t *testing.T) {
	f := newTeamFixture(t)
	now := time.Now()
	fs := &flakyStore{Store: f.bStore}
	b := newNetDaemon(t, "sam", "xps", fs, f.bTeam, f.bKeys, 0)

	dKeys, _ := identity.GenerateKeys()
	dBundle, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(dKeys, "pat", "tp"), false, now)
	if err != nil {
		t.Fatal(err)
	}
	send := func() envelope.Ack {
		ts := time.Now()
		payload, _ := json.Marshal(presence{Certs: []identity.Cert{dBundle.Cert}, TS: ts.UnixMilli()})
		env, _ := envelope.Build("presence", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, ts)
		return b.handleEnvelope(context.Background(), env)
	}
	persisted := func() bool {
		st, _ := team.Load(f.bStore)
		_, ok := st.CertFor(dKeys.Public())
		return ok
	}

	fs.fail.Store(true)
	if ack := send(); ack.State != message.StateDelivered {
		t.Fatalf("ack %+v", ack)
	}
	if persisted() {
		t.Fatal("persisted despite failing store")
	}
	fs.fail.Store(false)
	send() // same certs: Merge reports no change, but the pending save must still happen
	if !persisted() {
		t.Fatal("pending team change never persisted after the store recovered")
	}
}

func TestRetryAfterFromHintDoesNotSilenceMember(t *testing.T) {
	f := newTeamFixture(t)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(evil.Close)
	evilAddr := "lan://" + strings.TrimPrefix(evil.URL, "http://")
	// A's first send races the 429 endpoint against B's real address in the same phase.

	pa, pb := freePort(t), freePort(t)
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, pa, evilAddr, lanAddr(pb))
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, pb) // B learns A only from A's presence
	register(t, a, &fakeConn{id: 1}, "codex", "a1", "/tmp/alpha", false)
	runDaemon(t, b)
	runDaemon(t, a)
	waitUntil(t, 5*time.Second, "B sees A", func() bool { return statusOf(b, "alex/alpha-codex") == "idle" })

	register(t, a, &fakeConn{id: 2}, "codex", "a2", "/tmp/gamma", false)
	waitUntil(t, 5*time.Second, "B sees A's new session despite the 429 endpoint", func() bool {
		return statusOf(b, "alex/gamma-codex") == "idle"
	})
}

// fakeClock is a settable clock for daemon tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

// presenceFrom builds A's presence to B with envelope time envTS and payload TS payTS, listing sessions.
func presenceFrom(t *testing.T, f teamFixture, envTS, payTS time.Time, sessions ...string) envelope.Envelope {
	t.Helper()
	var es []WhoEntry
	for _, s := range sessions {
		es = append(es, WhoEntry{Address: "alex/" + s, Agent: "codex", Status: "idle"})
	}
	payload, _ := json.Marshal(presence{Sessions: es, TS: payTS.UnixMilli()})
	env, err := envelope.Build("presence", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, envTS)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestBackwardClockKeepsLivenessNotSnapshot(t *testing.T) {
	f := newTeamFixture(t)
	t0 := time.Now()
	clk := &fakeClock{now: t0}
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)
	b.opts.Now, b.verifier.Now, b.opts.OfflineAfter = clk.Now, clk.Now, 3*time.Minute

	if ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, t0, t0, "old")); ack.State != message.StateDelivered {
		t.Fatalf("first: %+v", ack)
	}
	// Two minutes later A's clock has stepped back 3 minutes: an older snapshot, but proof A is alive.
	clk.Set(t0.Add(2 * time.Minute))
	back := t0.Add(-time.Minute)
	if ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, back, back, "new")); ack.State != message.StateDelivered {
		t.Fatalf("second: %+v", ack)
	}
	clk.Set(t0.Add(4 * time.Minute))
	if got := statusOf(b, "alex/old"); got != "idle" {
		t.Fatalf("alex/old status = %q, want idle (liveness from receipt, newer snapshot kept)", got)
	}
	if got := statusOf(b, "alex/new"); got != "" {
		t.Fatalf("older snapshot replaced the directory: alex/new = %q", got)
	}
}

func TestMismatchedPayloadTimestampRefused(t *testing.T) {
	f := newTeamFixture(t)
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)
	now := time.Now()
	ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, now, now.Add(time.Hour), "pinned"))
	if ack.State != message.StateRefused || ack.Note != "malformed" {
		t.Fatalf("ack %+v, want refused/malformed", ack)
	}
	if statusOf(b, "alex/pinned") != "" {
		t.Fatal("mismatched presence reached the directory")
	}
	// A correct one afterwards is applied, so nothing was pinned.
	if ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, now, now, "ok")); ack.State != message.StateDelivered {
		t.Fatalf("ack %+v", ack)
	}
	if statusOf(b, "alex/ok") != "idle" {
		t.Fatal("valid presence not applied")
	}
}

func TestRefusalNotesArePublicReasons(t *testing.T) {
	f := newTeamFixture(t)
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)
	now := time.Now()

	env := presenceFrom(t, f, now, now, "x")
	if ack := b.handleEnvelope(context.Background(), env); ack.State != message.StateDelivered {
		t.Fatalf("ack %+v", ack)
	}
	if ack := b.handleEnvelope(context.Background(), env); ack.Note != "stale or replayed" {
		t.Fatalf("replay note = %q", ack.Note)
	}
	bad := presenceFrom(t, f, now, now, "x")
	bad.Sig[0] ^= 1
	if ack := b.handleEnvelope(context.Background(), bad); ack.Note != "bad signature" {
		t.Fatalf("bad sig note = %q", ack.Note)
	}
	_, epriv, _ := ed25519.GenerateKey(rand.Reader)
	eKeys, _ := identity.GenerateKeys()
	eTeam, _ := team.Create(storetest.SQLite(t), eKeys, epriv, "eve", "evil", now)
	foreign, _ := envelope.Build("presence", message.NewID(), eKeys, eTeam.Self, f.bTeam.Self, []byte("{}"), seal.HPKE{}, now)
	if ack := b.handleEnvelope(context.Background(), foreign); ack.Note != "not a member" {
		t.Fatalf("non-member note = %q", ack.Note)
	}
}

func TestExtremePayloadTimestampsRefused(t *testing.T) {
	f := newTeamFixture(t)
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)
	now := time.Now()
	if ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, now, now, "ok")); ack.State != message.StateDelivered {
		t.Fatalf("baseline: %+v", ack)
	}
	lastTS := func() int64 {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.peers[peerKey(f.aKeys.Public())].lastTS
	}
	before := lastTS()

	for _, tc := range []struct {
		name string
		ts   int64
	}{
		{"max int64", math.MaxInt64},
		{"min int64", math.MinInt64},
		{"wraps to zero in ns", now.UnixMilli() + 288230376151711744},
		{"negative", -1},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(presence{Sessions: []WhoEntry{{Address: "alex/pinned", Status: "idle"}}, TS: tc.ts})
			env, err := envelope.Build("presence", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ack := b.handleEnvelope(context.Background(), env); ack.State != message.StateRefused || ack.Note != "malformed" {
				t.Fatalf("ack %+v, want refused/malformed", ack)
			}
			if got := lastTS(); got != before {
				t.Fatalf("lastTS = %d, want unchanged %d", got, before)
			}
			if statusOf(b, "alex/pinned") != "" || statusOf(b, "alex/ok") != "idle" {
				t.Fatal("directory changed by a refused presence")
			}
		})
	}
}
