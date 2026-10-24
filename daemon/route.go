package daemon

import (
	"cmp"
	"fmt"

	"github.com/purnachand-jaddu/agentnet/message"
)

// target is one resolved recipient of a send: its mailbox key (the session's stable key, never reused) and display name.
type target struct {
	name string
	key  string
	peer string // remote machine's peerKey ("" if local); when set, name is the full address and key is peer/session
}

// resolveTo parses to and resolves it to the target(s) a send from fromKey should reach.
func (d *Daemon) resolveTo(fromKey, to string) ([]target, error) {
	addr, err := message.ParseAddress(to)
	if err != nil {
		return nil, err
	}
	if (addr.Person != "" && addr.Person != d.opts.Person) || (addr.Machine != "" && addr.Machine != d.opts.Machine) {
		addr.Person = cmp.Or(addr.Person, d.opts.Person)
		return d.resolveRemote(fromKey, addr)
	}
	if addr.IsGroup() {
		if fromKey != "cli:terminal" {
			return nil, fmt.Errorf("only the terminal can message a group")
		}
		targets := d.liveTargetsExcept(fromKey)
		if len(targets) == 0 {
			return nil, fmt.Errorf("no live sessions to message")
		}
		return targets, nil
	}
	d.mu.Lock()
	l := d.liveByName(addr.Session)
	d.mu.Unlock()
	if l != nil {
		return []target{{name: l.name, key: l.s.Key}}, nil
	}
	rec, err := d.opts.Store.SessionByName(addr.Session)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no such session %q", addr.Session)
	}
	return []target{{name: rec.Name, key: rec.Key}}, nil
}

// liveTargetsExcept returns every live session except exceptKey.
func (d *Daemon) liveTargetsExcept(exceptKey string) []target {
	d.mu.Lock()
	defer d.mu.Unlock()
	var targets []target
	for key, l := range d.live {
		if key == exceptKey {
			continue
		}
		targets = append(targets, target{name: l.name, key: key})
	}
	return targets
}

// liveByName returns the live session named name, if any. Caller holds mu.
func (d *Daemon) liveByName(name string) *live {
	for _, l := range d.live {
		if l.name == name {
			return l
		}
	}
	return nil
}
