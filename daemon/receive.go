package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
)

const (
	rateWindow    = time.Minute
	rateMax       = 60        // messages one machine may send per rateWindow before it is told to retry
	msgFromNS     = "msgfrom" // kv namespace: message id -> peerKey of the machine that sent it
	noSessionNote = "no such session"
	storeRetry    = "could not store it here right now; try again"
)

// receiveMsg verifies, checks and stores one "msg" envelope, then hands it to local deliver, which never waits on agent I/O.
func (d *Daemon) receiveMsg(e envelope.Envelope) (message.State, string) {
	m, to, err := d.openMsg(e)
	if err != nil {
		d.opts.Log("daemon: refused message from %s: %v", e.Cert.Machine, err)
		return message.StateRefused, publicReason(err)
	}
	d.recvMu.Lock()
	tg, st, note := d.admitMsg(e.Cert, m, to)
	d.recvMu.Unlock()
	if tg == nil {
		return st, note
	}
	r := d.deliver(tg.key, tg.name, []message.Message{m}, m.Wake)
	return r.State, r.Note
}

// openMsg verifies e (without the in-memory replay set), decodes its message and checks it against the envelope and sender cert.
func (d *Daemon) openMsg(e envelope.Envelope) (message.Message, message.Address, error) {
	v := d.verifier
	v.Replay = nil // admitMsg's per-sender store dedupe replaces it, so a retry after a lost ack is re-acked
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

// admitMsg dedupes m per sender, runs the receive checks and stores m; tg is set only when m was stored for delivery now. Caller holds recvMu.
func (d *Daemon) admitMsg(cert identity.Cert, m message.Message, to message.Address) (tg *target, st message.State, note string) {
	sender := peerKey(cert.Key)
	prev, _, err := d.opts.Store.MessageByID(m.ID)
	if err != nil {
		d.opts.Log("daemon: look up message %s: %v", m.ID, err)
		return nil, message.StateRetry, storeRetry
	}
	if prev != nil {
		from, err := d.opts.Store.Get(msgFromNS, m.ID)
		if err != nil {
			d.opts.Log("daemon: look up sender of %s: %v", m.ID, err)
			return nil, message.StateRetry, storeRetry
		}
		if string(from) != sender {
			d.opts.Log("daemon: refused message %s from %s: that id is already stored from another sender", m.ID, cert.Machine)
			return nil, message.StateRefused, envelope.ErrMalformed.Error()
		}
		return nil, message.StateDelivered, "already received"
	}
	if d.opts.Paused != nil && d.opts.Paused() {
		return nil, message.StateRefused, "paused"
	}
	if !d.rateAllowed(sender) {
		return nil, message.StateRetry, "too many messages from this machine; try again shortly"
	}
	terminal := m.From == cert.Person+"/terminal"
	if to.IsGroup() && !terminal {
		return nil, message.StateRefused, "only the terminal can message a group"
	}
	tg, err = d.localSession(to)
	if err != nil {
		d.opts.Log("daemon: look up session %q: %v", to.Session, err)
		return nil, message.StateRetry, storeRetry
	}
	if tg == nil {
		return nil, message.StateRefused, noSessionNote
	}
	if !terminal && !d.loopAllowed(sender+"/"+m.From, tg.key) {
		return nil, message.StateRefused, loopNote
	}
	st, note = d.inboundPolicy(cert, m)
	if st == message.StateRefused {
		return nil, st, note
	}
	held := st == message.StateHeld
	if err := d.opts.Store.Put(msgFromNS, m.ID, []byte(sender)); err != nil { // before the message, so a stored one always has its sender
		d.opts.Log("daemon: record sender of %s: %v", m.ID, err)
		return nil, message.StateRetry, storeRetry
	}
	if _, err := d.opts.Store.PutMessage(m, tg.key, held); err != nil {
		d.opts.Log("daemon: store message %s: %v", m.ID, err)
		return nil, message.StateRetry, storeRetry
	}
	if held {
		return nil, st, note
	}
	return tg, "", ""
}

// inboundPolicy decides whether m from cert is accepted (delivered), held or refused; it accepts everything until Task 24.
func (d *Daemon) inboundPolicy(identity.Cert, message.Message) (message.State, string) {
	return message.StateDelivered, ""
}

// localSession returns to's session on this machine, live or remembered, or nil if there is none.
func (d *Daemon) localSession(to message.Address) (*target, error) {
	if to.Person != d.opts.Person || (to.Machine != "" && to.Machine != d.opts.Machine) {
		return nil, nil
	}
	var tg *target
	d.mu.Lock()
	if l := d.liveByName(to.Session); l != nil {
		tg = &target{name: l.name, key: l.s.Key}
	}
	d.mu.Unlock()
	if tg != nil {
		return tg, nil
	}
	rec, err := d.opts.Store.SessionByName(to.Session)
	if err != nil || rec == nil {
		return nil, err
	}
	return &target{name: rec.Name, key: rec.Key}, nil
}

// rateAllowed reports whether machine sender may send another message within rateWindow, recording it if so.
func (d *Daemon) rateAllowed(sender string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ok bool
	d.rate[sender], ok = admitIn(d.rate[sender], d.opts.Now(), rateWindow, rateMax)
	return ok
}
