package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/transport"
)

// maxTSMismatchMS is how far a presence payload's TS may differ from its envelope's, in ms.
const maxTSMismatchMS = 5000

// sendTimeout bounds one presence send to one member across all its addresses; verified ones get the first half.
const sendTimeout = 5 * time.Second

// presence is what a machine tells every other member about itself.
type presence struct {
	Sessions []WhoEntry       `json:"sessions"` // its local sessions only
	Addrs    []string         `json:"addrs"`    // its transport addresses
	Certs    []identity.Cert  `json:"certs"`    // members it knows
	Blocks   []identity.Block `json:"blocks"`
	TS       int64            `json:"ts"` // unix ms
}

// startNetwork listens on every transport, starts discovery and the gossip loop; each failure is logged, never fatal.
func (d *Daemon) startNetwork(ctx context.Context) {
	var addrs []string
	for _, t := range d.opts.Transports {
		a, err := t.Listen(ctx, d.handleEnvelope)
		if err != nil {
			d.opts.Log("daemon: listen %v: %v; serving locally only", t.Schemes(), err)
			continue
		}
		addrs = append(addrs, a...)
	}
	d.mu.Lock()
	d.myAddrs = addrs
	d.mu.Unlock()

	teamID := identity.TeamID(d.opts.Team.Founder)
	port := portOf(addrs)
	for _, disc := range d.opts.Discoverers {
		if port != 0 {
			if err := disc.Announce(ctx, d.opts.Keys.Public(), teamID, port); err != nil {
				d.opts.Log("daemon: discovery announce unavailable: %v", err)
			}
		}
		d.netGo(func() {
			if err := disc.Browse(ctx, teamID, d.found); err != nil {
				d.opts.Log("daemon: discovery browse unavailable: %v", err)
			}
		})
	}
	d.netGo(func() { d.gossip(ctx) })
	d.netGo(func() { d.outboxLoop(ctx) })
}

// stopNetwork closes every transport and waits for the network goroutines; Run's ctx is already done.
func (d *Daemon) stopNetwork() {
	for _, t := range d.opts.Transports {
		if err := t.Close(); err != nil {
			d.opts.Log("daemon: close transport %v: %v", t.Schemes(), err)
		}
	}
	d.netWG.Wait()
}

// netGo runs fn via safeGo, tracked so stopNetwork waits for it.
func (d *Daemon) netGo(fn func()) {
	d.netWG.Add(1)
	d.safeGo(func() {
		defer d.netWG.Done()
		fn()
	})
}

// portOf returns the port of the first address that has one, or 0.
func portOf(addrs []string) int {
	for _, a := range addrs {
		if u, err := url.Parse(a); err == nil {
			if p, err := strconv.Atoi(u.Port()); err == nil {
				return p
			}
		}
	}
	return 0
}

// gossip sends presence when local sessions change (checked every Debounce), when netDirty is set, and every PresenceEvery.
func (d *Daemon) gossip(ctx context.Context) {
	tick := time.NewTicker(d.opts.Debounce)
	defer tick.Stop()
	var lastSnap []byte
	var lastSent time.Time
	for {
		sessions := d.localWho(WhoArgs{}).Sessions
		slices.SortFunc(sessions, func(x, y WhoEntry) int { return strings.Compare(x.Address, y.Address) })
		snap, _ := json.Marshal(sessions) // WhoEntry always marshals
		d.mu.Lock()
		dirty := d.netDirty
		d.netDirty = false
		d.mu.Unlock()
		d.persistTeam()
		if now := d.opts.Now(); dirty || !bytes.Equal(snap, lastSnap) || now.Sub(lastSent) >= d.opts.PresenceEvery {
			lastSnap, lastSent = snap, now
			d.broadcast(ctx, sessions)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// broadcast seals sessions, this machine's addresses and its team view to every other member, concurrently.
func (d *Daemon) broadcast(ctx context.Context, sessions []WhoEntry) {
	d.mu.Lock()
	addrs := slices.Clone(d.myAddrs)
	d.mu.Unlock()
	members := d.opts.Team.MembersSnapshot()
	payload, err := json.Marshal(presence{Sessions: sessions, Addrs: addrs, Certs: members,
		Blocks: d.opts.Team.BlocksSnapshot(), TS: d.opts.Now().UnixMilli()})
	if err != nil {
		d.opts.Log("daemon: presence: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range members {
		if bytes.Equal(c.Key, d.opts.Keys.Public()) {
			continue
		}
		wg.Add(1)
		d.safeGo(func() {
			defer wg.Done()
			d.sendPresence(ctx, c, payload)
		})
	}
	wg.Wait()
}

// sendPresence delivers payload to one member.
func (d *Daemon) sendPresence(ctx context.Context, to identity.Cert, payload []byte) {
	e, err := envelope.Build("presence", message.NewID(), d.opts.Keys, d.opts.Team.Self, to, payload, d.opts.Sealer, d.opts.Now())
	if err != nil {
		d.opts.Log("daemon: build presence for %s: %v", to.Machine, err)
		return
	}
	d.sendEnvelope(ctx, to, e)
}

// sendEnvelope sends e to member to, verified addresses first, then hints and static peers; it returns to's first valid signed ack.
func (d *Daemon) sendEnvelope(ctx context.Context, to identity.Cert, e envelope.Envelope) (envelope.Ack, bool) {
	key := peerKey(to.Key)
	verified, fallback := d.addrsFor(key, d.opts.Now())
	if len(verified)+len(fallback) == 0 {
		return envelope.Ack{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	first, cancelFirst := context.WithTimeout(ctx, sendTimeout/2)
	addr, ack, ok := d.race(first, to, e, verified)
	cancelFirst()
	if !ok {
		addr, ack, ok = d.race(ctx, to, e, fallback)
	}
	if ok {
		d.markVerified(key, addr)
	}
	return ack, ok
}

// race sends e to every addr at once and returns the first address whose reply is to's validly signed ack, with that ack.
func (d *Daemon) race(ctx context.Context, to identity.Cert, e envelope.Envelope, addrs []string) (string, envelope.Ack, bool) {
	if len(addrs) == 0 {
		return "", envelope.Ack{}, false
	}
	type win struct {
		addr string
		ack  envelope.Ack
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	won := make(chan win, len(addrs))
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Add(1)
		d.safeGo(func() {
			defer wg.Done()
			ack, err := d.sendVia(ctx, a, e)
			var ra *transport.RetryAfterError
			switch {
			case errors.As(err, &ra):
				d.backoff(a, ra.After)
			case err != nil:
			case envelope.VerifyAck(ack, to, e.ID) == nil:
				won <- win{a, ack}
			}
		})
	}
	d.safeGo(func() {
		wg.Wait()
		close(won)
	})
	w, ok := <-won
	return w.addr, w.ack, ok
}

// sendVia sends e to addr over the transport that dials addr's scheme.
func (d *Daemon) sendVia(ctx context.Context, addr string, e envelope.Envelope) (envelope.Ack, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return envelope.Ack{}, err
	}
	for _, t := range d.opts.Transports {
		if slices.Contains(t.Schemes(), u.Scheme) {
			return t.Send(ctx, addr, e)
		}
	}
	return envelope.Ack{}, fmt.Errorf("daemon: no transport for %q", addr)
}

// handleEnvelope answers one incoming envelope with a signed ack.
func (d *Daemon) handleEnvelope(_ context.Context, e envelope.Envelope) envelope.Ack {
	ack := envelope.Ack{ID: e.ID, State: message.StateDelivered, TS: d.opts.Now().UnixMilli()}
	switch e.Kind {
	case "presence":
		if err := d.receivePresence(e); err != nil {
			d.opts.Log("daemon: refused presence from %s: %v", e.Cert.Machine, err)
			ack.State, ack.Note = message.StateRefused, publicReason(err)
		}
	case "msg":
		ack.State, ack.Note = d.receiveMsg(e)
	default:
		ack.State, ack.Note = message.StateRefused, "not yet supported"
	}
	envelope.SignAck(&ack, d.opts.Keys)
	return ack
}

// receivePresence verifies and applies one presence: team merge (persisted on change), then the directory.
func (d *Daemon) receivePresence(e envelope.Envelope) error {
	plain, err := d.verifier.Open(e, d.opts.Keys, d.opts.Sealer)
	if err != nil {
		if errors.Is(err, envelope.ErrStale) {
			d.recordTimeRefusal(peerKey(e.Cert.Key))
		}
		return err
	}
	var p presence
	if err := json.Unmarshal(plain, &p); err != nil {
		return fmt.Errorf("presence: %v: %w", err, envelope.ErrMalformed)
	}
	now := d.opts.Now()
	if !tsConsistent(p.TS, e.TS, now) {
		return fmt.Errorf("presence: payload ts %d vs envelope ts %d: %w", p.TS, e.TS, envelope.ErrMalformed)
	}
	if d.opts.Team.Merge(append(p.Certs, e.Cert), p.Blocks, now) {
		d.unsaved.Store(true)
	}
	d.persistTeam()
	d.dropBlocked()                                         // a block just merged from gossip drops that machine here too, not just where it originated
	if err := d.opts.Team.Verify(e.Cert, now); err != nil { // its own presence may carry its block
		return fmt.Errorf("presence: %w: %w", envelope.ErrNotMember, err)
	}
	d.applyPresence(e.Cert, p, now)
	return nil
}

// teamChanged handles the "team_changed" op: reload Team from the store (e.g. after an out-of-band
// `agentnet revoke`) and gossip it immediately rather than waiting for the next presence tick.
func (d *Daemon) teamChanged(ctx context.Context) error {
	if d.opts.Team == nil {
		return errors.New("daemon: not in a network")
	}
	if err := d.opts.Team.Reload(d.opts.Store); err != nil {
		return err
	}
	d.dropBlocked()
	d.broadcast(ctx, d.localWho(WhoArgs{}).Sessions)
	return nil
}

// persistTeam saves the team if a merged change hasn't reached the store yet; a failure stays pending for the next call.
func (d *Daemon) persistTeam() {
	if !d.unsaved.Swap(false) {
		return
	}
	if err := d.opts.Team.Persist(d.opts.Store); err != nil {
		d.unsaved.Store(true)
		d.opts.Log("daemon: persist team (will retry): %v", err)
	}
}

// publicReason maps a refusal to the stable reason told to the caller; the detail is only logged.
func publicReason(err error) string {
	for _, r := range []error{envelope.ErrNotMember, envelope.ErrBadSignature, envelope.ErrStale, envelope.ErrMalformed} {
		if errors.Is(err, r) {
			return r.Error()
		}
	}
	return envelope.ErrMalformed.Error()
}

// tsConsistent reports whether payload and envelope ms timestamps are both in (0, now+1 day] and within maxTSMismatchMS; bounds first, so the subtraction can't overflow.
func tsConsistent(payTS, envTS int64, now time.Time) bool {
	limit := now.Add(24 * time.Hour).UnixMilli()
	if payTS <= 0 || envTS <= 0 || payTS > limit || envTS > limit {
		return false
	}
	diff := payTS - envTS
	return diff <= maxTSMismatchMS && diff >= -maxTSMismatchMS
}
