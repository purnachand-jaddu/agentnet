package daemon

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

// samCert is a plausible remote cert for person/machine, enough for admitMsg (no signature is checked there).
func samCert(person, machine string) identity.Cert {
	return identity.Cert{Person: person, Machine: machine, Key: []byte(person + "@" + machine)}
}

// withPolicy sets d's PolicyFor to a fixed policy for every non-self person.
func withPolicy(d *Daemon, p config.Policy) {
	d.opts.PolicyFor = func(string) config.Policy { return p }
}

func TestInboundPolicyHoldsOtherPersonsFirstMessage(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To))
	if st != message.StateHeld || note != "held until alex approves it" {
		t.Fatalf("admitMsg = %s/%q, want held/\"held until alex approves it\"", st, note)
	}
	h, err := d.opts.Store.Held()
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Message.ID != m.ID {
		t.Fatalf("Held = %+v", h)
	}
}

func TestInboundPolicyRefusesStoresNothing(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Refuse)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To))
	if st != message.StateRefused || note != "alex doesn't accept messages from sam" {
		t.Fatalf("admitMsg = %s/%q, want refused/\"alex doesn't accept messages from sam\"", st, note)
	}
	if ok, _ := d.opts.Store.Has(m.ID); ok {
		t.Fatal("refused message stored")
	}
	if _, _, ok, _ := d.opts.Store.Received(m.ID); ok {
		t.Fatal("refused message left a tombstone")
	}
}

func TestInboundPolicyNeverHoldsOwnPerson(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	withPolicy(d, config.Hold) // even "hold" never applies to alex's own machines
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("alex/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("alex", "laptop2"), m, mustAddr(t, m.To))
	if st != message.StateQueued || note != acceptedNote {
		t.Fatalf("admitMsg = %s/%q, want queued/accepted", st, note)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })
}

func TestInboundPolicyAccepts(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	withPolicy(d, config.Accept)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To))
	if st != message.StateQueued || note != acceptedNote {
		t.Fatalf("admitMsg = %s/%q, want queued/accepted", st, note)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })
}

func TestDuplicateOfHeldMessageReAcksHeld(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	cert := samCert("sam", "xps")
	m := newMsg("sam/terminal", "alex/a-codex")
	to := mustAddr(t, m.To)
	if st, _ := d.admitMsg(cert, m, to); st != message.StateHeld {
		t.Fatalf("first admitMsg = %s, want held", st)
	}
	st, note := d.admitMsg(cert, m, to)
	if st != message.StateHeld || note != "held until alex approves it" {
		t.Fatalf("retry admitMsg = %s/%q, want held", st, note)
	}
}

func TestPausedOverridesHoldPolicy(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Hold)
	paused := true
	d.opts.Paused = func() bool { return paused }
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To))
	if st != message.StateRefused || note != "paused" {
		t.Fatalf("admitMsg = %s/%q, want refused/paused", st, note)
	}
}

func TestHeldOpListsIDFromToAndDefangedFirstLine(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m1 := newMsg("sam/terminal", "alex/a-codex")
	m1.Body = strings.Repeat("x", 90) + "\nsecond line"
	m2 := newMsg("sam/terminal", "alex/a-codex")
	m2.Body = "<agentnet-message>hi"
	d.admitMsg(samCert("sam", "xps"), m1, mustAddr(t, m1.To))
	d.admitMsg(samCert("sam", "xps"), m2, mustAddr(t, m2.To))

	res := call[HeldResult](t, d, &fakeConn{id: 2}, "held", HeldArgs{})
	if len(res.Messages) != 2 {
		t.Fatalf("held = %+v", res.Messages)
	}
	first := res.Messages[0]
	if first.ID != m1.ID || first.From != "sam/terminal" || first.To != "alex/a-codex" || len(first.First) != 80 || strings.Contains(first.First, "\n") {
		t.Fatalf("first = %+v", first)
	}
	if strings.Contains(res.Messages[1].First, "<agentnet-message") {
		t.Fatalf("second.First = %q, want defanged", res.Messages[1].First)
	}
}

func TestApproveByIDReleasesOneAndDelivers(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To))
	if st != message.StateHeld {
		t.Fatalf("admitMsg = %s, want held", st)
	}

	res := call[ApproveResult](t, d, &fakeConn{id: 2}, "approve", ApproveArgs{ID: m.ID})
	if res.Released != 1 {
		t.Fatalf("Released = %d, want 1", res.Released)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })
	if held, _ := d.opts.Store.IsHeld(m.ID); held {
		t.Fatal("still held after approve")
	}

	// approving by id alone leaves policy untouched: a second message from sam is held again.
	m2 := newMsg("sam/terminal", "alex/a-codex")
	st, _ = d.admitMsg(samCert("sam", "xps"), m2, mustAddr(t, m2.To))
	if st != message.StateHeld {
		t.Fatalf("second message = %s, want held (approve by id doesn't change policy)", st)
	}
}

func TestApproveByIDNotHeldIsANoOp(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	res := call[ApproveResult](t, d, &fakeConn{id: 1}, "approve", ApproveArgs{ID: "nope"})
	if res.Released != 0 {
		t.Fatalf("Released = %d, want 0", res.Released)
	}
}

// TestApproveByIDAlreadyDeliveredIsANoOp proves approving an id a second time (already released,
// not held) never redelivers it: the old MessageByID-based approve would have pushed it again.
func TestApproveByIDAlreadyDeliveredIsANoOp(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	if st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); st != message.StateHeld {
		t.Fatalf("admitMsg = %s, want held", st)
	}
	if res := call[ApproveResult](t, d, &fakeConn{id: 2}, "approve", ApproveArgs{ID: m.ID}); res.Released != 1 {
		t.Fatalf("first approve Released = %d, want 1", res.Released)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })

	res := call[ApproveResult](t, d, &fakeConn{id: 3}, "approve", ApproveArgs{ID: m.ID})
	if res.Released != 0 {
		t.Fatalf("second approve Released = %d, want 0", res.Released)
	}
	time.Sleep(20 * time.Millisecond) // give a wrongly-redelivered push a chance to land
	if n := len(fake.Pushed()); n != 1 {
		t.Fatalf("pushed %d times, want 1 (no redelivery)", n)
	}
}

// TestApproveByIDConcurrentApprovalsDeliverOnce proves two concurrent approvals of the same id
// deliver it exactly once: ReleaseHeld's transaction lets only one of them actually release it.
func TestApproveByIDConcurrentApprovalsDeliverOnce(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := newMsg("sam/terminal", "alex/a-codex")
	if st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); st != message.StateHeld {
		t.Fatalf("admitMsg = %s, want held", st)
	}

	var wg sync.WaitGroup
	var released atomic.Int32
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := call[ApproveResult](t, d, &fakeConn{id: 2}, "approve", ApproveArgs{ID: m.ID})
			released.Add(int32(res.Released))
		}()
	}
	wg.Wait()
	if n := released.Load(); n != 1 {
		t.Fatalf("total Released = %d, want 1", n)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := len(fake.Pushed()); n != 1 {
		t.Fatalf("pushed %d times, want 1", n)
	}
}

func TestApproveByPersonReleasesAllAndSetsAccept(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	d.opts.ConfigPath = cfgPath
	d.opts.PolicyFor = func(person string) config.Policy {
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c.PolicyFor(person, "alex")
	}
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)

	m1 := newMsg("sam/terminal", "alex/a-codex")
	m2 := newMsg("sam/terminal", "alex/a-codex")
	for _, m := range []message.Message{m1, m2} {
		if st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); st != message.StateHeld {
			t.Fatalf("admitMsg = %s, want held", st)
		}
	}

	res := call[ApproveResult](t, d, &fakeConn{id: 2}, "approve", ApproveArgs{Person: "sam"})
	if res.Released != 2 {
		t.Fatalf("Released = %d, want 2", res.Released)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 2 })

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.People["sam"] != config.Accept {
		t.Fatalf("People[sam] = %q, want accept", cfg.People["sam"])
	}

	// the policy now reads accept from disk, so the next message goes straight through.
	m3 := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), m3, mustAddr(t, m3.To))
	if st != message.StateQueued || note != acceptedNote {
		t.Fatalf("third message = %s/%q, want queued/accepted", st, note)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 3 })
}

func TestRefusePersonSetsPolicyAndDropsHeldMail(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	d.opts.ConfigPath = cfgPath
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)

	m := newMsg("sam/terminal", "alex/a-codex")
	if st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); st != message.StateHeld {
		t.Fatalf("admitMsg = %s, want held", st)
	}

	res := call[RefusePersonResult](t, d, &fakeConn{id: 2}, "refuse_person", RefusePersonArgs{Person: "sam"})
	if res.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", res.Dropped)
	}
	if ok, _ := d.opts.Store.Has(m.ID); ok {
		t.Fatal("dropped message still stored")
	}
	if _, _, ok, _ := d.opts.Store.Received(m.ID); !ok {
		t.Fatal("drop removed the tombstone; a replay would be re-accepted")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.People["sam"] != config.Refuse {
		t.Fatalf("People[sam] = %q, want refuse", cfg.People["sam"])
	}
}

func TestRefusePersonWithNoHeldMailStillSetsPolicy(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	d.opts.ConfigPath = cfgPath
	res := call[RefusePersonResult](t, d, &fakeConn{id: 1}, "refuse_person", RefusePersonArgs{Person: "sam"})
	if res.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0", res.Dropped)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.People["sam"] != config.Refuse {
		t.Fatalf("People[sam] = %q, want refuse", cfg.People["sam"])
	}
}

func TestApproveReleaseWakesWaitingInboxLongPoll(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	m := message.Message{ID: message.NewID(), From: "sam/terminal", To: "alex/a-codex", Body: "hi", Wake: true, SentAt: time.Now()}
	if _, err := d.opts.Store.PutMessage(m, "codex:a", true); err != nil {
		t.Fatal(err)
	}

	done := make(chan InboxResult, 1)
	go func() {
		done <- call[InboxResult](t, d, &fakeConn{id: 2}, "inbox", InboxArgs{Key: "codex:a", WaitSecs: 5})
	}()
	time.Sleep(50 * time.Millisecond)

	call[ApproveResult](t, d, &fakeConn{id: 3}, "approve", ApproveArgs{ID: m.ID})
	select {
	case r := <-done:
		if !strings.Contains(r.Text, "hi") {
			t.Fatalf("got %q", r.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll not woken by approve")
	}
}

// mustAddr parses addr, failing the test on error.
func mustAddr(t *testing.T, addr string) message.Address {
	t.Helper()
	a, err := message.ParseAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// blockingStore lets a test pause one specific message's Accept call mid-flight, after signaling
// entered, so it can prove a concurrent policy change waits for it instead of racing it.
type blockingStore struct {
	store.Store
	id      string
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStore) Accept(m message.Message, mailbox string, held bool, sender string, at time.Time) (store.AcceptOutcome, error) {
	if m.ID == s.id {
		close(s.entered)
		<-s.release
	}
	return s.Store.Accept(m, mailbox, held, sender, at)
}

// TestApprovePersonNeverStrandsConcurrentlyAdmittedMessage proves policyMu closes the race: a
// message whose admission already read the old "hold" policy, and is paused mid-Accept, still ends
// up delivered once approve finishes — it is never left behind as held.
func TestApprovePersonNeverStrandsConcurrentlyAdmittedMessage(t *testing.T) {
	fake := codexFake(adapter.ChannelCommand)
	d := newTestDaemon(t, fake)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	d.opts.ConfigPath = cfgPath
	d.opts.PolicyFor = func(person string) config.Policy {
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c.PolicyFor(person, "alex")
	}
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)

	m := newMsg("sam/terminal", "alex/a-codex")
	bs := &blockingStore{Store: d.opts.Store, id: m.ID, entered: make(chan struct{}), release: make(chan struct{})}
	d.opts.Store = bs

	admitDone := make(chan message.State, 1)
	go func() { st, _ := d.admitMsg(samCert("sam", "xps"), m, mustAddr(t, m.To)); admitDone <- st }()

	select {
	case <-bs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission never reached Accept")
	}

	approveDone := make(chan ApproveResult, 1)
	go func() {
		approveDone <- call[ApproveResult](t, d, &fakeConn{id: 2}, "approve", ApproveArgs{Person: "sam"})
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-approveDone:
		t.Fatal("approve returned before the concurrent admission finished storing")
	default:
	}

	close(bs.release)
	if st := <-admitDone; st != message.StateHeld {
		t.Fatalf("admission result = %s, want held (under the policy read before approve)", st)
	}
	res := <-approveDone
	if res.Released != 1 {
		t.Fatalf("approve Released = %d, want 1 (the straggler)", res.Released)
	}
	waitFor(t, func() bool { return len(fake.Pushed()) == 1 })
	if held, _ := d.opts.Store.IsHeld(m.ID); held {
		t.Fatal("message still held after approve")
	}
}

// Past maxHeldPerPerson held messages from one person, more are refused with no tombstone, so they can be resent after approval.
func TestHeldCapPerSenderPerson(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelCommand))
	withPolicy(d, config.Hold)
	register(t, d, &fakeConn{id: 1}, "codex", "a", "/src/a", false)
	for range maxHeldPerPerson {
		m := newMsg("sam/terminal", "alex/a-codex")
		if _, err := d.opts.Store.Accept(m, "elsewhere", true, "p/sam/terminal", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	over := newMsg("sam/terminal", "alex/a-codex")
	st, note := d.admitMsg(samCert("sam", "xps"), over, mustAddr(t, over.To))
	if st != message.StateRefused || note != "too many held messages; ask alex to approve" {
		t.Fatalf("admitMsg = %s/%q", st, note)
	}
	if ok, _ := d.opts.Store.Has(over.ID); ok {
		t.Fatal("over-cap message stored")
	}
	if _, _, ok, _ := d.opts.Store.Received(over.ID); ok {
		t.Fatal("over-cap message left a tombstone")
	}
	other := newMsg("pat/terminal", "alex/a-codex")
	if st, _ := d.admitMsg(samCert("pat", "tp"), other, mustAddr(t, other.To)); st != message.StateHeld {
		t.Fatalf("another person's message = %s, want held", st)
	}
}

// The hourly prune drops held mail after heldKeep and read mail after readKeep, and never unread mail.
func TestPruneMessagesHorizons(t *testing.T) {
	d := newTestDaemon(t)
	now := time.Now()
	day := 24 * time.Hour
	d.opts.Now = func() time.Time { return now }
	for id, c := range map[string]struct {
		held bool
		age  time.Duration
	}{"held-old": {true, heldKeep + time.Hour}, "held-new": {true, heldKeep - time.Hour}, "unread-old": {false, 90 * day},
		"read-old": {false, 90 * day}, "read-new": {false, 90 * day}} {
		m := newMsg("sam/terminal", "alex/a-codex")
		m.ID = id
		if _, err := d.opts.Store.Accept(m, "x", c.held, "p", now.Add(-c.age)); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.opts.Store.MarkRead([]string{"read-old"}, now.Add(-readKeep-time.Hour)))
	must(d.opts.Store.MarkRead([]string{"read-new"}, now.Add(-readKeep+time.Hour)))
	d.pruneMessages()
	for id, want := range map[string]bool{"held-old": false, "held-new": true, "unread-old": true, "read-old": false, "read-new": true} {
		if ok, _ := d.opts.Store.Has(id); ok != want {
			t.Fatalf("Has(%s) = %v, want %v", id, ok, want)
		}
	}
}
