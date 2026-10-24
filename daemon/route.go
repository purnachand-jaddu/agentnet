package daemon

import (
	"fmt"

	"github.com/purnachand-jaddu/agentnet/message"
)

// target is one resolved recipient of a send: its session name, live or remembered.
type target struct {
	name string
}

// resolveTo parses to and resolves it to the target(s) a send from fromKey should reach.
func (d *Daemon) resolveTo(fromKey, to string) ([]target, error) {
	addr, err := message.ParseAddress(to)
	if err != nil {
		return nil, err
	}
	if addr.Person != "" && addr.Person != d.opts.Person {
		return nil, fmt.Errorf("no such person on this machine (remote delivery comes later)")
	}
	if addr.IsGroup() {
		if fromKey != "cli:terminal" {
			return nil, fmt.Errorf("only the terminal can message a group")
		}
		return d.liveTargetsExcept(fromKey), nil
	}
	d.mu.Lock()
	l := d.liveByName(addr.Session)
	d.mu.Unlock()
	if l != nil {
		return []target{{name: l.name}}, nil
	}
	rec, err := d.opts.Store.SessionByName(addr.Session)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no such session %q", addr.Session)
	}
	return []target{{name: rec.Name}}, nil
}

// liveTargetsExcept returns every live session's name except exceptKey.
func (d *Daemon) liveTargetsExcept(exceptKey string) []target {
	d.mu.Lock()
	defer d.mu.Unlock()
	var targets []target
	for key, l := range d.live {
		if key == exceptKey {
			continue
		}
		targets = append(targets, target{name: l.name})
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
