package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

const (
	rateWindow       = time.Minute
	rateMax          = 60                 // messages one machine may send per rateWindow before it is told to retry
	receivedKeep     = 8 * 24 * time.Hour // tombstones outlive the sender's 7-day outbox
	heldKeep         = 8 * 24 * time.Hour // held mail nobody approved is dropped after this
	readKeep         = 30 * 24 * time.Hour
	maxHeldPerPerson = 100 // held messages one sender person may have waiting here
	noSessionNote    = "no such session"
	storeRetry       = "could not store it here right now; try again"
	acceptedNote     = "accepted; delivering"
	alreadyNote      = "already received"
)

// receiveMsg verifies, checks and stores one "msg" envelope; delivery runs in the background, so the ack never waits on agent I/O.
func (d *Daemon) receiveMsg(e envelope.Envelope) (message.State, string) {
	m, to, err := d.openMsg(e)
	if err != nil {
		if errors.Is(err, envelope.ErrClockSkew) { // signature already verified; a real time-window miss
			d.recordTimeRefusal(peerKey(e.Cert.Key))
		}
		reason := publicReason(err)
		if skipped, ok := d.refusalDue(); ok {
			d.opts.Log("daemon: refused message from %q: %v%s", e.Cert.Machine, err, skippedNote(skipped))
			d.auditRefused(e.ID, peerKey(e.Cert.Key), reason+skippedNote(skipped))
		}
		return message.StateRefused, reason
	}
	st, note := d.admitMsg(e.Cert, m, to)
	d.audit("in", m.ID, m.From, m.To, st, m.Body)
	return st, note
}

// refusalDue reports whether a refused envelope may be logged now (the first each minute), and how many were skipped before it.
// ponytail: one limit for all senders, since the source IP isn't visible here and the claimed key is forgeable.
func (d *Daemon) refusalDue() (skipped int, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.opts.Now()
	if now.Sub(d.refusedAt) < time.Minute {
		d.refusedSkipped++
		return 0, false
	}
	skipped = d.refusedSkipped
	d.refusedAt, d.refusedSkipped = now, 0
	return skipped, true
}

// skippedNote is the suffix telling a log or audit reader how many refusals weren't recorded before this one.
func skippedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d more refused since the last one recorded)", n)
}

// openMsg verifies e (without the in-memory replay set), decodes its message and checks it against the envelope and sender cert.
func (d *Daemon) openMsg(e envelope.Envelope) (message.Message, message.Address, error) {
	v := d.verifier
	v.Replay = nil // the store's durable tombstones replace it, so a retry after a lost ack is re-acked
	var m message.Message
	plain, err := v.Open(e, d.opts.Keys, d.opts.Sealer)
	if err != nil {
		return m, message.Address{}, err
	}
	if err := json.Unmarshal(plain, &m); err != nil {
		return m, message.Address{}, fmt.Errorf("msg: %v: %w", err, envelope.ErrMalformed)
	}
	if m.ID != e.ID { // Open already required e.ID to be a valid ULID
		return m, message.Address{}, fmt.Errorf("msg: id %q in envelope %q: %w", m.ID, e.ID, envelope.ErrMalformed)
	}
	if from, err := message.ParseAddress(m.From); err != nil || from.Person != e.Cert.Person || from.Machine != "" {
		return m, message.Address{}, fmt.Errorf("msg: from %q is not one of %s's sessions: %w", m.From, e.Cert.Person, envelope.ErrMalformed)
	}
	to, err := message.ParseAddress(m.To)
	if err != nil {
		return m, message.Address{}, fmt.Errorf("msg: %v: %w", err, envelope.ErrMalformed)
	}
	if m.Body == "" || len(m.Body) > message.MaxBody {
		return m, message.Address{}, fmt.Errorf("msg: body length %d: %w", len(m.Body), envelope.ErrMalformed)
	}
	return m, to, nil
}

// admitMsg checks m's recipient, dedupes it against its tombstone, runs the receive checks, stores it and starts delivery.
func (d *Daemon) admitMsg(cert identity.Cert, m message.Message, to message.Address) (message.State, string) {
	terminal := m.From == cert.Person+"/terminal"
	if to.Person != d.opts.Person || (to.Machine != "" && to.Machine != d.opts.Machine) {
		return message.StateRefused, noSessionNote
	}
	if to.IsGroup() && !terminal {
		return message.StateRefused, "only the terminal can message a group"
	}
	machine := peerKey(cert.Key)
	sender := machine + "/" + m.From
	mu := d.admitLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	prevSender, prevTo, seen, err := d.opts.Store.Received(m.ID)
	if err != nil {
		d.opts.Log("daemon: look up message %s: %v", m.ID, err)
		return message.StateRetry, storeRetry
	}
	if seen {
		return d.duplicateAck(cert, m.ID, prevSender == sender && prevTo == m.To)
	}
	if d.opts.Paused != nil && d.opts.Paused() {
		return message.StateRefused, "paused"
	}
	if !d.rateAllowed(machine) {
		return message.StateRetry, "too many messages from this machine; try again shortly"
	}
	tg, err := d.localSession(to)
	if err != nil {
		d.opts.Log("daemon: look up session %q: %v", to.Session, err)
		return message.StateRetry, storeRetry
	}
	if tg == nil {
		return message.StateRefused, noSessionNote
	}
	// RLock spans the policy read through the store write, so a concurrent policy change (Lock)
	// can't finish while this admission is still mid-flight under the old policy.
	d.policyMu.RLock()
	defer d.policyMu.RUnlock()
	st, note := d.inboundPolicy(cert, m)
	if st == message.StateRefused {
		return st, note
	}
	if st == message.StateHeld {
		// ponytail: count then insert isn't atomic, so concurrent admissions can overshoot the cap by a few.
		n, err := d.opts.Store.HeldFrom(cert.Person)
		if err != nil {
			d.opts.Log("daemon: count held from %s: %v", cert.Person, err)
			return message.StateRetry, storeRetry
		}
		if n >= maxHeldPerPerson {
			return message.StateRefused, fmt.Sprintf("too many held messages; ask %s to approve", d.opts.Person)
		}
	}
	if !terminal && !d.loopAllowed(sender, tg.key) {
		return message.StateRefused, loopNote
	}
	out, err := d.opts.Store.Accept(m, tg.key, st == message.StateHeld, sender, d.opts.Now())
	if err != nil || out != store.Inserted {
		if !terminal {
			d.loopRelease(sender, tg.key) // the budget counts stored messages only
		}
		if err != nil {
			d.opts.Log("daemon: store message %s: %v", m.ID, err)
			return message.StateRetry, storeRetry
		}
		return d.duplicateAck(cert, m.ID, out == store.Duplicate)
	}
	if st == message.StateHeld {
		return st, note
	}
	d.safeGo(func() { d.deliver(tg.key, tg.name, []message.Message{m}, m.Wake) })
	return message.StateQueued, acceptedNote
}

// admitLock returns the mutex serialising admission (tombstone lookup through Accept) of every message with id.
func (d *Daemon) admitLock(id string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(id))
	return &d.admitMu[h.Sum32()%uint32(len(d.admitMu))]
}

// duplicateAck answers an id already stored: its own sender's retry is re-acked with its current state
// (held or delivered); anything else is refused and logged. A held lookup failure answers retry.
func (d *Daemon) duplicateAck(cert identity.Cert, id string, same bool) (message.State, string) {
	if !same {
		d.opts.Log("daemon: refused message %s from %s: that id was received from another sender or for another recipient", id, cert.Machine)
		return message.StateRefused, envelope.ErrMalformed.Error()
	}
	held, err := d.opts.Store.IsHeld(id)
	if err != nil {
		d.opts.Log("daemon: look up held %s: %v", id, err)
		return message.StateRetry, storeRetry
	}
	if held {
		return message.StateHeld, heldNote(d.opts.Person)
	}
	return message.StateDelivered, alreadyNote
}

// pruneReceived drops message tombstones older than receivedKeep.
func (d *Daemon) pruneReceived() {
	if _, err := d.opts.Store.PruneReceived(d.opts.Now().Add(-receivedKeep)); err != nil {
		d.opts.Log("daemon: prune received: %v", err)
	}
}

// pruneMessages drops held mail older than heldKeep and read mail older than readKeep; unread mail is kept.
func (d *Daemon) pruneMessages() {
	now := d.opts.Now()
	n, err := d.opts.Store.PruneMessages(now.Add(-heldKeep), now.Add(-readKeep))
	if err != nil {
		d.opts.Log("daemon: prune messages: %v", err)
		return
	}
	if n > 0 {
		d.opts.Log("daemon: pruned %d old held or read messages", n)
	}
}

// localSession returns to's session on this machine, live or remembered, or nil if there is none or its folder is opted out.
func (d *Daemon) localSession(to message.Address) (*target, error) {
	if to.Person != d.opts.Person || (to.Machine != "" && to.Machine != d.opts.Machine) {
		return nil, nil
	}
	var tg *target
	var cwd string
	d.mu.Lock()
	if l := d.liveByName(to.Session); l != nil {
		tg, cwd = &target{name: l.name, key: l.s.Key}, l.s.Cwd
	}
	d.mu.Unlock()
	if tg == nil {
		rec, err := d.opts.Store.SessionByName(to.Session)
		if err != nil || rec == nil {
			return nil, err
		}
		tg, cwd = &target{name: rec.Name, key: rec.Key}, rec.Cwd
	}
	if !d.opts.FolderAllowed(cwd) {
		return nil, nil // its folder was opted out since it registered
	}
	return tg, nil
}

// rateAllowed reports whether machine sender (a peerKey) may send another message within rateWindow, recording it if so.
func (d *Daemon) rateAllowed(sender string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ok bool
	d.rate[sender], ok = admitIn(d.rate[sender], d.opts.Now(), rateWindow, rateMax)
	return ok
}
