package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
)

const (
	unreachableNote = "not reachable on this network right now; it goes out when it is"
	outboxMinAge    = 20 * time.Second   // the retry loop resends only entries at least this old
	outboxBatch     = 50                 // most entries resent to one peer per flush
	outboxKeep      = 7 * 24 * time.Hour // queued entries older than this are dropped
)

// resolveRemote resolves addr (another person, or another machine) against the remote directory.
func (d *Daemon) resolveRemote(fromKey string, addr message.Address) ([]target, error) {
	if addr.IsGroup() && fromKey != "cli:terminal" {
		return nil, fmt.Errorf("only the terminal can message a group")
	}
	type hit struct {
		cert    identity.Cert
		session string
	}
	now := d.opts.Now()
	var hits []hit
	d.mu.Lock()
	for _, p := range d.peers {
		if p.cert.Person != addr.Person || (addr.Machine != "" && p.cert.Machine != addr.Machine) {
			continue
		}
		for _, s := range p.sessions {
			if (addr.IsGroup() && d.online(p, now)) || s.Address == addr.Session {
				hits = append(hits, hit{p.cert, s.Address})
			}
		}
	}
	d.mu.Unlock()
	hits = slices.DeleteFunc(hits, func(h hit) bool { // drop machines blocked since their presence
		_, member := d.opts.Team.CertFor(h.cert.Key)
		return !member
	})
	if len(hits) == 0 && addr.IsGroup() {
		return nil, fmt.Errorf("no live sessions to message")
	}
	if len(hits) == 0 {
		return nil, fmt.Errorf("no such session in the network")
	}

	machines := map[string]int{} // session name -> how many of the hits' machines have it
	for _, h := range hits {
		machines[h.session]++
	}
	var targets []target
	for _, h := range hits {
		name := addr.Person + "/" + h.session
		if addr.Machine != "" || machines[h.session] > 1 {
			name += "@" + h.cert.Machine
		}
		targets = append(targets, target{name: name, key: peerKey(h.cert.Key) + "/" + h.session, peer: peerKey(h.cert.Key)})
	}
	if len(targets) > 1 && !addr.IsGroup() {
		names := make([]string, len(targets))
		for i, tg := range targets {
			names[i] = tg.name
		}
		slices.Sort(names)
		return nil, fmt.Errorf("ambiguous: name one of %s", strings.Join(names, ", "))
	}
	return targets, nil
}

// sendRemote queues msg for tg's machine, then makes one attempt; ok is false if it couldn't be queued.
func (d *Daemon) sendRemote(ctx context.Context, tg target, msg message.Message) (r message.Receipt, ok bool) {
	r.To = tg.name
	mu := d.peerLock(tg.peer)
	mu.Lock()
	defer mu.Unlock()
	if err := d.opts.Store.OutboxAdd(tg.peer, msg, d.opts.Now()); err != nil {
		r.State, r.Note = message.StateRefused, fmt.Sprintf("could not store the message: %v", err)
		return r, false
	}
	cert, member := d.memberCert(tg.peer)
	if !member {
		r.State, r.Note = message.StateQueued, unreachableNote
		return r, true
	}
	r.State, r.Note, _ = d.attempt(ctx, cert, msg)
	return r, true
}

// attempt sends queued msg to cert once, removing it from the outbox unless no valid ack came back or the ack says retry. Caller holds the peer's lock.
func (d *Daemon) attempt(ctx context.Context, cert identity.Cert, msg message.Message) (st message.State, note string, done bool) {
	payload, err := json.Marshal(msg)
	if err != nil {
		d.opts.Log("daemon: encode message %s: %v", msg.ID, err)
		return message.StateQueued, unreachableNote, false
	}
	e, err := envelope.Build("msg", msg.ID, d.opts.Keys, d.opts.Team.Self, cert, payload, d.opts.Sealer, d.opts.Now())
	if err != nil {
		d.opts.Log("daemon: build message %s for %s: %v", msg.ID, cert.Machine, err)
		return message.StateQueued, unreachableNote, false
	}
	ack, ok := d.sendEnvelope(ctx, cert, e)
	if !ok {
		return message.StateQueued, unreachableNote, false
	}
	if ack.State == message.StateRetry {
		return ack.State, ack.Note, false
	}
	if err := d.opts.Store.OutboxDone(msg.ID); err != nil {
		d.opts.Log("daemon: outbox done %s: %v", msg.ID, err)
	}
	return ack.State, ack.Note, true
}

// peerLock returns the mutex that serialises message sends to peer.
func (d *Daemon) peerLock(peer string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.sendMu[peer]
	if !ok {
		m = &sync.Mutex{}
		d.sendMu[peer] = m
	}
	return m
}

// memberCert returns the current member cert for peer (a peerKey), if it is still a member.
func (d *Daemon) memberCert(peer string) (identity.Cert, bool) {
	k, err := base64.RawURLEncoding.DecodeString(peer)
	if err != nil {
		return identity.Cert{}, false
	}
	return d.opts.Team.CertFor(k)
}

// outboxLoop resends queued messages every OutboxEvery, and starts flushers whenever a flush is requested.
func (d *Daemon) outboxLoop(ctx context.Context) {
	tick := time.NewTicker(d.opts.OutboxEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.outboxTick(ctx)
		case <-d.kick:
			d.startFlushers(ctx)
		}
	}
}

// outboxTick drops entries older than outboxKeep, then flushes every peer's entries older than outboxMinAge.
func (d *Daemon) outboxTick(ctx context.Context) {
	now := d.opts.Now()
	if n, err := d.opts.Store.OutboxPrune(now.Add(-outboxKeep)); err != nil {
		d.opts.Log("daemon: outbox prune: %v", err)
	} else if n > 0 {
		d.opts.Log("daemon: dropped %d queued messages older than a week", n)
	}
	peers, err := d.opts.Store.OutboxPeers()
	if err != nil {
		d.opts.Log("daemon: outbox peers: %v", err)
		return
	}
	d.mu.Lock()
	for _, p := range peers {
		d.requestFlush(p, now.Add(-outboxMinAge))
	}
	d.mu.Unlock()
	d.startFlushers(ctx)
}

// requestFlush marks peer dirty for entries queued at or before cutoff and wakes the outbox loop. Caller holds mu.
func (d *Daemon) requestFlush(peer string, cutoff time.Time) {
	if cutoff.After(d.dirty[peer]) {
		d.dirty[peer] = cutoff
	}
	select {
	case d.kick <- struct{}{}:
	default: // a wake-up is already pending; it sees this peer too
	}
}

// startFlushers starts a flusher for every dirty peer that has none running.
func (d *Daemon) startFlushers(ctx context.Context) {
	d.mu.Lock()
	var start []string
	for p := range d.dirty {
		if !d.flushing[p] {
			d.flushing[p] = true
			start = append(start, p)
		}
	}
	d.mu.Unlock()
	for _, p := range start {
		d.netGo(func() { d.flusher(ctx, p) })
	}
}

// flusher flushes peer while it is dirty, clearing the flag at the start of each pass, so no request is lost.
func (d *Daemon) flusher(ctx context.Context, peer string) {
	for {
		d.mu.Lock()
		cutoff, dirty := d.dirty[peer]
		delete(d.dirty, peer)
		if !dirty || ctx.Err() != nil {
			delete(d.flushing, peer)
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		d.flushPeer(ctx, peer, cutoff)
	}
}

// flushPeer resends up to outboxBatch of peer's entries queued at or before cutoff, oldest first, stopping when the peer is unreachable.
func (d *Daemon) flushPeer(ctx context.Context, peer string, cutoff time.Time) {
	cert, ok := d.memberCert(peer)
	if !ok {
		return // blocked or unknown: its entries age out via OutboxPrune
	}
	mu := d.peerLock(peer)
	mu.Lock()
	defer mu.Unlock()
	msgs, err := d.opts.Store.OutboxFor(peer, cutoff)
	if err != nil {
		d.opts.Log("daemon: outbox for %s: %v", cert.Machine, err)
		return
	}
	for _, m := range msgs[:min(len(msgs), outboxBatch)] {
		if st, _, done := d.attempt(ctx, cert, m); !done && st != message.StateRetry {
			return // no valid ack: the rest would fail the same way
		}
	}
}
