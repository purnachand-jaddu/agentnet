package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
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
	if st != message.StateHeld || note != "held until sam approves it" {
		t.Fatalf("admitMsg = %s/%q, want held/\"held until sam approves it\"", st, note)
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
	if st != message.StateHeld || note != "held until sam approves it" {
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
