package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
	"github.com/purnachand-jaddu/agentnet/team"
)

// newAuditor opens a FileAuditor at a fresh path in t's temp dir, closing it at cleanup.
func newAuditor(t *testing.T) (*FileAuditor, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	au, err := NewFileAuditor(path, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { au.Close() })
	return au, path
}

// readAudit parses path's JSON lines into AuditEntry values.
func readAudit(t *testing.T, path string) []AuditEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditEntry
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuditFileMode0600(t *testing.T) {
	_, path := newAuditor(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestAuditRecordNeverCarriesBody(t *testing.T) {
	au, path := newAuditor(t)
	body := "the secret body nobody should see"
	au.Record(AuditEntry{TS: 1, Dir: "in", ID: "m1", From: "sam/terminal", To: "alex/a", State: message.StateHeld, BodySHA: bodyHash(body)})

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.BodySHA != bodyHash(body) || e.BodySHA == body {
		t.Fatalf("BodySHA = %q", e.BodySHA)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), body) {
		t.Fatal("audit file contains the body text")
	}
}

func TestAuditInboundRecordsOneLinePerDecision(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	b := newB(t, f, 0)
	b.d.opts.Auditor = au // PolicyFor stays nil: default accept, matching other receive_test.go scenarios

	m := newMsg("alex/terminal", "sam/web-codex")
	ack := fromA(t, f, b, m)
	if ack.State != message.StateQueued {
		t.Fatalf("ack %+v, want queued", ack)
	}

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.Dir != "in" || e.ID != m.ID || e.From != m.From || e.To != m.To || e.State != message.StateQueued {
		t.Fatalf("entry %+v", e)
	}
	if e.BodySHA != bodyHash(m.Body) {
		t.Fatalf("BodySHA = %q, want hash of body", e.BodySHA)
	}

	// a second, held decision appends its own extra line, not a duplicate of the first.
	withPolicy(b.d, config.Hold)
	held := newMsg("alex/terminal", "sam/web-codex")
	ack = fromA(t, f, b, held)
	if ack.State != message.StateHeld {
		t.Fatalf("ack %+v, want held", ack)
	}
	entries = readAudit(t, path)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}
	if entries[1].ID != held.ID || entries[1].State != message.StateHeld {
		t.Fatalf("entries[1] = %+v", entries[1])
	}
}

func TestAuditOutboundRecordsEachAttempt(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	aPort, bPort := freePort(t), freePort(t)
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, aPort, lanAddr(bPort))
	a.opts.Auditor = au
	b := startB(t, f, bPort, aPort)
	runDaemon(t, a)
	waitUntil(t, 5*time.Second, "A lists sam/web-codex", func() bool { return statusOf(a, "sam/web-codex") == "idle" })

	res := sendOne(t, a, "cli:terminal", "sam/web-codex")
	id := res.IDs[0]
	waitUntil(t, 2*time.Second, "B gets it", func() bool { return len(b.fake.Pushed()) == 1 })

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.Dir != "out" || e.ID != id || e.From != "alex/terminal" || e.To != "sam/web-codex" {
		t.Fatalf("entry %+v", e)
	}
	if e.BodySHA == "" || strings.Contains(e.BodySHA, "hello") {
		t.Fatalf("BodySHA = %q", e.BodySHA)
	}
}

func TestAuditLocalSendDeliveredRecorded(t *testing.T) {
	au, path := newAuditor(t)
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	d.opts.Auditor = au
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)

	res := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/a-codex", Body: "hi", Wake: true})
	if len(res.IDs) != 1 {
		t.Fatalf("res = %+v", res)
	}
	entries := readAudit(t, path)
	if len(entries) != 1 || entries[0].Dir != "out" || entries[0].ID != res.IDs[0] || entries[0].State != res.Receipts[0].State {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestAuditLocalSendStoreFailureRecorded(t *testing.T) {
	au, path := newAuditor(t)
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	regB := register(t, d, &fakeConn{id: 1}, "codex", "b", "/src/b", false)
	d.opts.Store = &failingStore{Store: d.opts.Store, failMailbox: regB.Key}
	d.opts.Auditor = au

	res := call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/b-codex", Body: "hi", Wake: true})
	if res.Receipts[0].State != message.StateRefused {
		t.Fatalf("receipt = %+v, want refused", res.Receipts[0])
	}
	entries := readAudit(t, path)
	if len(entries) != 1 || entries[0].Dir != "out" || entries[0].State != message.StateRefused || !message.ValidID(entries[0].ID) {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestAuditRemoteOutboxStorageFailureRecorded(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	fs := &failingOutboxStore{Store: f.aStore}
	a := newNetDaemon(t, "alex", "mbp", fs, f.aTeam, f.aKeys, 0)
	a.opts.Auditor = au
	bk := peerKey(f.bKeys.Public())
	tg := target{name: "sam/web-codex", key: bk + "/web-codex", peer: bk}
	msg := message.Message{ID: message.NewID(), From: "alex/terminal", To: "sam/web-codex", Body: "hi", SentAt: time.Now()}

	r, ok := a.sendRemote(context.Background(), tg, msg)
	if ok || r.State != message.StateRefused {
		t.Fatalf("sendRemote = %+v, %v, want refused/false", r, ok)
	}
	entries := readAudit(t, path)
	if len(entries) != 1 || entries[0].Dir != "out" || entries[0].ID != msg.ID || entries[0].State != message.StateRefused {
		t.Fatalf("entries = %+v", entries)
	}
}

// failingOutboxStore fails every OutboxAdd, so sendRemote never reaches attempt and must audit itself.
type failingOutboxStore struct{ store.Store }

func (s *failingOutboxStore) OutboxAdd(peer string, m message.Message, at time.Time) error {
	return errors.New("disk full")
}

func TestAuditRemoteNoMemberRecorded(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, 0)
	a.opts.Auditor = au
	peer := peerKey([]byte("not-a-real-member-of-this-team-"))
	tg := target{name: "ghost/x", key: peer + "/x", peer: peer}
	msg := message.Message{ID: message.NewID(), From: "alex/terminal", To: "ghost/x", Body: "hi", SentAt: time.Now()}

	r, ok := a.sendRemote(context.Background(), tg, msg)
	if !ok || r.State != message.StateQueued || r.Note != unreachableNote {
		t.Fatalf("sendRemote = %+v, %v, want queued/unreachable", r, ok)
	}
	entries := readAudit(t, path)
	if len(entries) != 1 || entries[0].Dir != "out" || entries[0].ID != msg.ID || entries[0].State != message.StateQueued {
		t.Fatalf("entries = %+v", entries)
	}
}

// TestAuditInboundPreDecryptionRefusalHasReasonNoBody proves a refusal before decryption succeeds
// (another founder's envelope, bad ciphertext) records the envelope id and claimed sender key, a
// reason, and no body hash at all — there's no trustworthy plaintext to hash.
func TestAuditInboundPreDecryptionRefusalHasReasonNoBody(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	b := newB(t, f, 0)
	b.d.opts.Auditor = au

	_, epriv, _ := ed25519.GenerateKey(rand.Reader)
	eKeys, _ := identity.GenerateKeys()
	eTeam, err := team.Create(storetest.SQLite(t), eKeys, epriv, "eve", "evil", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := sealMsg(t, eKeys, eTeam.Self, f.bTeam.Self, newMsg("eve/terminal", "sam/web-codex"))
	e.Sealed = []byte("not even ciphertext") // a cert check after decryption would wrongly answer "malformed"

	ack := handle(t, f, b, e)
	if ack.State != message.StateRefused {
		t.Fatalf("ack %+v, want refused", ack)
	}

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	got := entries[0]
	if got.Dir != "in" || got.ID != e.ID || got.To != "" || got.State != message.StateRefused {
		t.Fatalf("entry %+v", got)
	}
	if got.From != peerKey(eTeam.Self.Key) {
		t.Fatalf("From = %q, want the claimed sender's key", got.From)
	}
	if got.Reason == "" {
		t.Fatal("Reason is empty")
	}
	if got.BodySHA != "" {
		t.Fatalf("BodySHA = %q, want empty: no trustworthy plaintext", got.BodySHA)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "body_sha256") {
		t.Fatal("audit line includes body_sha256 for a pre-decryption refusal")
	}
}

// logLines captures d's log lines.
func logLines(d *Daemon) func() []string {
	var mu sync.Mutex
	var lines []string
	d.opts.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(lines) }
}

// An unverified cert.machine with a newline must not forge a line in daemon.log.
func TestRefusalLogQuotesUnverifiedMachine(t *testing.T) {
	f := newTeamFixture(t)
	b := newB(t, f, 0)
	lines := logLines(b.d)
	for _, kind := range []string{"msg", "presence"} {
		e := sealMsg(t, f.aKeys, f.aTeam.Self, f.bTeam.Self, newMsg("alex/terminal", "sam/web-codex"))
		e.Kind = kind
		e.Cert.Machine = "mbp\nFORGED"
		b.d.refusedAt = time.Time{} // let each refusal through the once-a-minute limit
		if ack := handle(t, f, b, e); ack.State != message.StateRefused {
			t.Fatalf("%s: ack %+v, want refused", kind, ack)
		}
	}
	got := lines()
	if len(got) != 2 {
		t.Fatalf("log = %q, want 2 lines", got)
	}
	for _, l := range got {
		if strings.Contains(l, "\nFORGED") {
			t.Fatalf("log line carries a raw newline: %q", l)
		}
	}
}

// Refused envelopes are logged and audited at most once a minute; the next report counts the ones skipped.
func TestRefusalsLoggedAndAuditedOncePerMinute(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	b := newB(t, f, 0)
	b.d.opts.Auditor = au
	lines := logLines(b.d)
	clk := &fakeClock{now: time.Now()}
	b.d.opts.Now = clk.Now
	garbage := func() envelope.Envelope {
		e := sealMsg(t, f.aKeys, f.aTeam.Self, f.bTeam.Self, newMsg("alex/terminal", "sam/web-codex"))
		e.Sig[0] ^= 1
		return e
	}
	for range 5 {
		handle(t, f, b, garbage())
	}
	if n := len(readAudit(t, path)); n != 1 {
		t.Fatalf("audit lines = %d, want 1", n)
	}
	if n := len(lines()); n != 1 {
		t.Fatalf("log lines = %q, want 1", lines())
	}
	clk.Set(clk.Now().Add(time.Minute + time.Second))
	handle(t, f, b, garbage())
	entries := readAudit(t, path)
	if len(entries) != 2 || !strings.Contains(entries[1].Reason, "4 more") {
		t.Fatalf("entries = %+v, want a second one counting 4 skipped", entries)
	}
	if l := lines(); len(l) != 2 || !strings.Contains(l[1], "4 more") {
		t.Fatalf("log = %q, want a second line counting 4 skipped", l)
	}
}

// Outbox retries that still fail are not audited; the final outcome is.
func TestAuditRetriesOnlyFinalOutcome(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	aPort, bPort := freePort(t), freePort(t)
	a := idleA(t, f, lanAddr(bPort)) // B isn't listening yet
	a.opts.Auditor = au
	bk := peerKey(f.bKeys.Public())
	m := queue(t, f.aStore, bk, "later", time.Now().Add(-time.Minute))

	for range 3 {
		a.flushPeer(context.Background(), bk, time.Now())
	}
	if entries := readAudit(t, path); len(entries) != 0 {
		t.Fatalf("failed retries audited: %+v", entries)
	}
	startB(t, f, bPort, aPort)
	a.flushPeer(context.Background(), bk, time.Now())
	entries := readAudit(t, path)
	if len(entries) != 1 || entries[0].ID != m.ID || entries[0].State != message.StateQueued {
		t.Fatalf("entries = %+v, want one final outcome for %s", entries, m.ID)
	}
}
