package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/message"
)

// pushTimeout bounds a daemon-side Adapter.Push call.
const pushTimeout = 30 * time.Second

// inFlightWindow is how long a pushed id stays hidden from Pending before it's assumed stuck.
const inFlightWindow = 90 * time.Second

// loopWindow and loopMax bound how many messages one session can send another before the loop guard refuses more.
const (
	loopWindow = 10 * time.Minute
	loopMax    = 12
)

// loopNote is the receipt note when the loop guard refuses a message.
const loopNote = "too many messages between these two sessions; is this a loop?"

// pullNote is the receipt note for a session that will pick its mail up on its own, with no push channel reaching it right now.
const pullNote = "it sees it when it checks its inbox"

// SendArgs is the "send" op's arguments.
type SendArgs struct {
	From    string `json:"from"` // session key, or "cli:terminal"
	To      string `json:"to"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
	Wake    bool   `json:"wake"`
}

// SendResult answers a "send" op: one id and receipt per recipient.
type SendResult struct {
	IDs      []string          `json:"ids"`
	Receipts []message.Receipt `json:"receipts"`
}

// PushFailedArgs is the "push_failed" op's arguments: an MCP process reporting it couldn't deliver ids.
type PushFailedArgs struct {
	Key string   `json:"key"`
	IDs []string `json:"ids"`
}

// send handles the "send" op: validates, resolves recipients, stores one message per recipient and delivers it.
func (d *Daemon) send(ctx context.Context, a SendArgs) (SendResult, error) {
	if a.Body == "" {
		return SendResult{}, fmt.Errorf("empty message body")
	}
	if len(a.Body) > message.MaxBody {
		return SendResult{}, fmt.Errorf("message body too long (max %d bytes)", message.MaxBody)
	}
	fromAddr, fromAgent, err := d.senderAddress(a.From)
	if err != nil {
		return SendResult{}, err
	}
	targets, err := d.resolveTo(a.From, a.To)
	if err != nil {
		return SendResult{}, err
	}

	var res SendResult
	for _, tg := range targets {
		to := d.opts.Person + "/" + tg.name
		if tg.peer != "" {
			to = tg.name
		}
		if a.From != "cli:terminal" && !d.loopAllowed(a.From, tg.key) {
			res.Receipts = append(res.Receipts, message.Receipt{To: to, State: message.StateRefused, Note: loopNote})
			continue
		}
		msg := message.Message{
			ID: message.NewID(), From: fromAddr, To: to, FromAgent: fromAgent,
			ReplyTo: a.ReplyTo, Body: a.Body, Wake: a.Wake, SentAt: d.opts.Now(),
		}
		if tg.peer != "" {
			r, ok := d.sendRemote(ctx, tg, msg)
			if ok {
				res.IDs = append(res.IDs, msg.ID)
			}
			res.Receipts = append(res.Receipts, r)
			continue
		}
		if _, err := d.opts.Store.PutMessage(msg, tg.key, false); err != nil {
			st := message.StateRefused
			res.Receipts = append(res.Receipts, message.Receipt{To: to, State: st,
				Note: fmt.Sprintf("could not store the message: %v", err)})
			d.audit("out", msg.ID, msg.From, msg.To, st, msg.Body)
			continue
		}
		res.IDs = append(res.IDs, msg.ID)
		r := d.deliver(tg.key, tg.name, []message.Message{msg}, a.Wake)
		res.Receipts = append(res.Receipts, r)
		d.audit("out", msg.ID, msg.From, msg.To, r.State, msg.Body)
	}
	return res, nil
}

// senderAddress resolves fromKey to the address and adapter name to stamp on an outgoing message.
func (d *Daemon) senderAddress(fromKey string) (addr, agentName string, err error) {
	if fromKey == "cli:terminal" {
		return d.opts.Person + "/terminal", "", nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.live[fromKey]
	if !ok {
		return "", "", fmt.Errorf("unknown sender %q", fromKey)
	}
	return d.opts.Person + "/" + l.name, l.s.Agent, nil
}

// loopAllowed reports whether another message from "from" to "to" is allowed within loopWindow, recording this attempt if so.
func (d *Daemon) loopAllowed(from, to string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := [2]string{from, to}
	var ok bool
	d.pairs[key], ok = admitIn(d.pairs[key], d.opts.Now(), loopWindow, loopMax)
	return ok
}

// loopRelease gives back one loopAllowed reservation for the pair, for a message that wasn't stored.
func (d *Daemon) loopRelease(from, to string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := [2]string{from, to}
	if n := len(d.pairs[key]); n > 0 {
		d.pairs[key] = d.pairs[key][:n-1]
	}
}

// admitIn drops ts entries at or before now-window, then appends now if fewer than limit remain, reporting whether it did.
func admitIn(ts []time.Time, now time.Time, window time.Duration, limit int) ([]time.Time, bool) {
	cutoff := now.Add(-window)
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		return kept, false
	}
	return append(kept, now), true
}

// pending returns mailbox's unread messages, hiding ids pushed within inFlightWindow.
func (d *Daemon) pending(mailbox string) ([]message.Message, error) {
	msgs, err := d.opts.Store.Pending(mailbox)
	if err != nil {
		return nil, err
	}
	now := d.opts.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []message.Message
	for _, m := range msgs {
		if t, ok := d.inFlight[m.ID]; ok {
			if now.Sub(t) < inFlightWindow {
				continue
			}
			delete(d.inFlight, m.ID)
		}
		out = append(out, m)
	}
	return out, nil
}

// deliver gets msgs (all addressed to the session keyed by key) to it now, returning a receipt for the recipient (shown as name).
func (d *Daemon) deliver(key, name string, msgs []message.Message, wake bool) message.Receipt {
	to := d.opts.Person + "/" + name
	d.mu.Lock()
	waiting := d.waiting[key] > 0
	d.broadcastArrived()
	if waiting {
		d.mu.Unlock()
		return message.Receipt{To: to, State: message.StateDelivered, Note: "delivered: the session is waiting for messages"}
	}
	l := d.live[key]
	if l == nil {
		d.mu.Unlock()
		return message.Receipt{To: to, State: message.StateQueued, Note: "it reaches that session when it's back"}
	}
	if !wake {
		d.mu.Unlock()
		return message.Receipt{To: to, State: message.StateQueued, Note: "FYI: read at its next pause"}
	}
	a, ok := d.opts.Adapters.Get(l.s.Agent)
	if !ok {
		d.mu.Unlock()
		return message.Receipt{To: to, State: message.StateQueued, Note: pullNote}
	}
	st := adapter.State{MCPPush: l.mcpPush, HookSeen: l.hookSeen, PushFailed: l.pushFailed}
	sess, conn := l.s, l.conn
	ch := a.Channel(sess, st)
	ids := idsOf(msgs)
	if ch == adapter.ChannelMCP || ch == adapter.ChannelCommand {
		now := d.opts.Now()
		for _, id := range ids {
			d.inFlight[id] = now
		}
	}
	d.mu.Unlock()

	switch ch {
	case adapter.ChannelMCP:
		if conn == nil {
			d.clearInFlight(ids)
			return message.Receipt{To: to, State: message.StateQueued, Note: pullNote}
		}
		if err := conn.Push(ipc.Event{Event: "deliver", Key: sess.Key, IDs: ids, Text: message.ForAgent(msgs)}); err != nil {
			d.opts.Log("daemon: push event to %s: %v", sess.Key, err)
			d.clearInFlight(ids)
			return message.Receipt{To: to, State: message.StateQueued, Note: pullNote}
		}
		return message.Receipt{To: to, State: message.StatePushed}
	case adapter.ChannelCommand:
		d.pushInBackground(a, sess, message.ForAgent(msgs), ids)
		return message.Receipt{To: to, State: message.StatePushed}
	case adapter.ChannelHook:
		d.startTurnWatch(key, sess, a)
		return message.Receipt{To: to, State: message.StateQueued, Note: "it sees it at its next tool call or when its turn ends"}
	default: // ChannelPull
		return message.Receipt{To: to, State: message.StateQueued, Note: pullNote}
	}
}

// broadcastArrived wakes every long-polling "inbox" call, for any key, then replaces d.arrived for the next one. Caller holds mu.
func (d *Daemon) broadcastArrived() {
	close(d.arrived)
	d.arrived = make(chan struct{})
}

// clearInFlight drops ids from in-flight tracking, e.g. after a push attempt that never reached the session.
func (d *Daemon) clearInFlight(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		delete(d.inFlight, id)
	}
}

// idsOf returns the ids of msgs.
func idsOf(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// pushInBackground runs a.Push for sess in a goroutine, recording the outcome on its live session and marking ids read on success.
func (d *Daemon) pushInBackground(a adapter.Adapter, sess adapter.Session, text string, ids []string) {
	d.safeGo(func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		err := a.Push(ctx, sess, text)

		d.mu.Lock()
		if l, ok := d.live[sess.Key]; ok {
			l.pushFailed = err != nil
		}
		for _, id := range ids {
			delete(d.inFlight, id) // either it's read below, or the fallback (hook/pull) must see it right away
		}
		d.mu.Unlock()

		if err != nil {
			d.opts.Log("daemon: push to %s failed: %v", sess.Key, err)
			return
		}
		if err := d.opts.Store.MarkRead(ids, d.opts.Now()); err != nil {
			d.opts.Log("daemon: mark read %v: %v", ids, err)
		}
	})
}

// safeGo runs fn in its own goroutine, recovering and logging any panic instead of crashing the daemon.
func (d *Daemon) safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.opts.Log("daemon: panic in background push: %v", r)
			}
		}()
		fn()
	}()
}

// pushFailed handles the "push_failed" op: an MCP process reporting it has no inbox for ids, so they stay unread.
func (d *Daemon) pushFailed(a PushFailedArgs) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l, ok := d.live[a.Key]; ok {
		l.mcpPush = false
	}
	for _, id := range a.IDs {
		delete(d.inFlight, id)
	}
}
