package daemon

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
)

const (
	maxHints      = 8                 // unverified hints kept per machine, so spoofed discovery can't grow memory
	maxAddrs      = 16                // addresses accepted from one presence
	maxRetryAfter = 300 * time.Second // longest a peer's Retry-After can make us skip it
)

// peer is one remote member machine as this daemon knows it.
type peer struct {
	cert     identity.Cert // from its latest presence
	sessions []WhoEntry    // its local sessions; Address is the bare session name
	verified []string      // from its own signed presence, or an address that produced its signed ack
	hints    []string      // unverified, from discovery
	lastSeen time.Time     // when its latest presence arrived
	lastTS   int64         // its latest presence timestamp (unix ms); older ones are ignored
	skew     time.Duration // now minus its presence timestamp, for doctor
}

// peerKey is a machine key as the peers map key.
func peerKey(k []byte) string { return base64.RawURLEncoding.EncodeToString(k) }

// peerFor returns key's peer, creating it if needed. Caller holds mu.
func (d *Daemon) peerFor(key string) *peer {
	p, ok := d.peers[key]
	if !ok {
		p = &peer{}
		d.peers[key] = p
	}
	return p
}

// online reports whether p's latest presence is recent enough.
func (d *Daemon) online(p *peer, now time.Time) bool {
	return !p.lastSeen.IsZero() && now.Sub(p.lastSeen) < d.opts.OfflineAfter
}

// found records a discovery hint: keyless ones as static peers, keyed ones only for known members.
func (d *Daemon) found(f discovery.Found) {
	if len(f.Key) == 0 {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, a := range f.Addrs {
			if !slices.Contains(d.statics, a) {
				d.statics = append(d.statics, a)
				d.netDirty = true
			}
		}
		return
	}
	if bytes.Equal(f.Key, d.opts.Keys.Public()) {
		return
	}
	if _, ok := d.opts.Team.CertFor(f.Key); !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.peerFor(peerKey(f.Key))
	for _, a := range f.Addrs {
		if slices.Contains(p.verified, a) || slices.Contains(p.hints, a) {
			continue
		}
		p.hints = append(p.hints, a)
		if len(p.hints) > maxHints {
			p.hints = p.hints[1:]
		}
		d.netDirty = true
	}
}

// applyPresence marks from alive as of now, and replaces its directory entries and verified addresses unless p is older.
func (d *Daemon) applyPresence(from identity.Cert, p presence, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pr := d.peerFor(peerKey(from.Key))
	if !d.online(pr, now) {
		d.netDirty = true // it just (re)appeared: tell it about us now
	}
	pr.cert, pr.lastSeen, pr.skew = from, now, now.Sub(time.UnixMilli(p.TS))
	if p.TS <= pr.lastTS {
		return // an older snapshot (e.g. its clock stepped back) still proves liveness
	}
	pr.lastTS = p.TS
	pr.sessions = cleanSessions(p.Sessions)
	pr.verified = cleanAddrs(p.Addrs)
	pr.hints = slices.DeleteFunc(pr.hints, func(a string) bool { return slices.Contains(pr.verified, a) })
}

// cleanSessions keeps a peer's well-named entries, reduced to the bare session name with status busy or idle.
func cleanSessions(in []WhoEntry) []WhoEntry {
	var out []WhoEntry
	for _, e := range in {
		name := e.Address[strings.LastIndex(e.Address, "/")+1:]
		if !message.Slug(name) {
			continue
		}
		if e.Status != "busy" {
			e.Status = "idle"
		}
		e.Address, e.You = name, false
		out = append(out, e)
	}
	return out
}

// cleanAddrs keeps up to maxAddrs distinct scheme://host addresses.
func cleanAddrs(in []string) []string {
	var out []string
	for _, a := range in {
		u, err := url.Parse(a)
		if err != nil || u.Scheme == "" || u.Host == "" || slices.Contains(out, a) {
			continue
		}
		out = append(out, a)
		if len(out) == maxAddrs {
			break
		}
	}
	return out
}

// addrsFor returns where to reach key: verified addresses, then hints and static peers, minus endpoints backing off.
func (d *Daemon) addrsFor(key string, now time.Time) (verified, fallback []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.peerFor(key)
	for _, a := range p.verified {
		if d.usable(a, now) {
			verified = append(verified, a)
		}
	}
	for _, a := range append(slices.Clone(p.hints), d.statics...) {
		if d.usable(a, now) && !slices.Contains(p.verified, a) && !slices.Contains(fallback, a) {
			fallback = append(fallback, a)
		}
	}
	return verified, fallback
}

// usable reports whether addr isn't backing off, forgetting expired backoffs. Caller holds mu.
func (d *Daemon) usable(addr string, now time.Time) bool {
	until, ok := d.addrSkip[addr]
	if ok && !now.Before(until) {
		delete(d.addrSkip, addr)
		ok = false
	}
	return !ok
}

// markVerified records that addr produced key's signed ack.
func (d *Daemon) markVerified(key, addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.peerFor(key)
	if !slices.Contains(p.verified, addr) {
		p.verified = append(p.verified, addr)
	}
	p.hints = slices.DeleteFunc(p.hints, func(a string) bool { return a == addr })
}

// backoff skips the endpoint addr for after (clamped to maxRetryAfter); other addresses of the same machine are unaffected.
func (d *Daemon) backoff(addr string, after time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrSkip[addr] = d.opts.Now().Add(min(after, maxRetryAfter))
}

// remoteWho lists remote sessions as person/session, adding @machine where that person has the name on two machines.
func (d *Daemon) remoteWho(local []WhoEntry) []WhoEntry {
	if d.opts.Team == nil {
		return nil
	}
	type row struct {
		e       WhoEntry
		key     []byte
		machine string
	}
	now := d.opts.Now()
	var rows []row
	d.mu.Lock()
	for _, p := range d.peers {
		for _, e := range p.sessions {
			if !d.online(p, now) {
				e.Status = "offline"
			}
			e.Address = p.cert.Person + "/" + e.Address
			rows = append(rows, row{e, p.cert.Key, p.cert.Machine})
		}
	}
	d.mu.Unlock()

	machines := map[string]map[string]bool{} // person/session -> machines that have it
	add := func(addr, machine string) {
		if machines[addr] == nil {
			machines[addr] = map[string]bool{}
		}
		machines[addr][machine] = true
	}
	for _, e := range local {
		add(e.Address, d.opts.Machine)
	}
	rows = slices.DeleteFunc(rows, func(r row) bool { // drop machines blocked since their presence
		_, member := d.opts.Team.CertFor(r.key)
		return !member
	})
	for _, r := range rows {
		add(r.e.Address, r.machine)
	}
	out := make([]WhoEntry, 0, len(rows))
	for _, r := range rows {
		if len(machines[r.e.Address]) > 1 {
			r.e.Address += "@" + r.machine
		}
		out = append(out, r.e)
	}
	return out
}
