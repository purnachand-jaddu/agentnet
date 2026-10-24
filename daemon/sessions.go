package daemon

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store"
)

// hookGrace is how long a session's hooks keep it alive after its connection drops.
const hookGrace = 10 * time.Minute

// live is one session the daemon currently tracks in memory.
type live struct {
	s          adapter.Session
	name       string
	conn       ipc.Conn
	mcpPush    bool
	hookSeen   bool
	pushFailed bool
	busy       bool
	lastHook   time.Time
	statusAt   time.Time
}

var invalidNameChar = regexp.MustCompile(`[^a-z0-9-]`)

// slugBase turns cwd's base directory name into the first part of a session name.
func slugBase(cwd string) string {
	b := strings.ToLower(filepath.Base(cwd))
	b = invalidNameChar.ReplaceAllString(b, "-")
	b = strings.TrimLeft(b, "-")
	if b == "" {
		b = "session"
	}
	return b
}

// buildName joins base and short with an -N suffix for n>1, truncating base so the result stays within 40 chars.
func buildName(base, short string, n int) string {
	suffix := ""
	if n > 1 {
		suffix = fmt.Sprintf("-%d", n)
	}
	tail := "-" + short + suffix
	max := 40 - len(tail)
	if max < 1 {
		max = 1
	}
	if len(base) > max {
		base = base[:max]
	}
	return base + tail
}

// register handles the "register" op: naming, process-join and the remembered record. Caller must not hold mu.
func (d *Daemon) register(c ipc.Conn, a RegisterArgs) (Registered, error) {
	if !d.opts.FolderAllowed(a.Session.Cwd) {
		return Registered{}, fmt.Errorf("agentnet is off for this folder; run agentnet folder add %s", a.Session.Cwd)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conns[c.ID()] = c

	// Session join by process: an MCP connection attaches to a live hook-registered session of the same agent and PID.
	if a.MCPPID != 0 {
		for _, l := range d.live {
			if l.s.Agent == a.Session.Agent && l.s.PID == a.MCPPID {
				l.conn = c
				if a.MCPPush {
					l.mcpPush = true
				}
				return Registered{Key: l.s.Key, Address: d.opts.Person + "/" + l.name, Name: l.name}, nil
			}
		}
	}

	key := a.Session.Key
	l, exists := d.live[key]
	if !exists {
		name, err := d.assignName(key, a.Session.Agent, a.Session.Cwd)
		if err != nil {
			return Registered{}, err
		}
		l = &live{s: a.Session, name: name}
		d.live[key] = l
		rec := store.SessionRecord{Name: name, Key: key, Agent: a.Session.Agent, Cwd: a.Session.Cwd, LastSeen: d.opts.Now()}
		if err := d.opts.Store.UpsertSession(rec); err != nil {
			delete(d.live, key)
			return Registered{}, err
		}
	} else {
		l.s = a.Session
	}
	l.conn = c
	if a.MCPPush {
		l.mcpPush = true
	}
	return Registered{Key: key, Address: d.opts.Person + "/" + l.name, Name: l.name}, nil
}

// assignName picks the name for key: its remembered name if still free, else the first free <base>-<short>[-N]. Caller holds mu.
func (d *Daemon) assignName(key, agentName, cwd string) (string, error) {
	rec, err := d.opts.Store.SessionByKey(key)
	if err != nil {
		return "", err
	}
	if rec != nil && d.nameFree(key, rec.Name) {
		return rec.Name, nil
	}
	a, ok := d.opts.Adapters.Get(agentName)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	base, short := slugBase(cwd), a.Short()
	for n := 1; ; n++ {
		name := buildName(base, short, n)
		if d.nameFree(key, name) {
			return name, nil
		}
	}
}

// nameFree reports whether name is unused by any live session or remembered record of a different key. Caller holds mu.
func (d *Daemon) nameFree(key, name string) bool {
	for k, l := range d.live {
		if k != key && l.name == name {
			return false
		}
	}
	rec, err := d.opts.Store.SessionByName(name)
	if err != nil {
		d.opts.Log("daemon: SessionByName(%q): %v", name, err)
		return false // fail closed: UpsertSession's ON CONFLICT(name) would steal the name otherwise
	}
	return rec == nil || rec.Key == key
}

// unregister drops key from live unless its hooks ran within hookGrace; the remembered record is untouched.
func (d *Daemon) unregister(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.live[key]
	if !ok {
		return
	}
	if d.opts.Now().Sub(l.lastHook) >= hookGrace {
		delete(d.live, key)
	}
}

// ConnClosed detaches c from any session it served, removing the session if c was its only link and its hooks haven't run recently.
func (d *Daemon) ConnClosed(c ipc.Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.conns, c.ID())
	now := d.opts.Now()
	for key, l := range d.live {
		if l.conn != c {
			continue
		}
		l.conn = nil
		l.mcpPush = false
		if now.Sub(l.lastHook) >= hookGrace {
			delete(d.live, key)
		}
	}
}
