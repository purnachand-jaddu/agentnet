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

// maxNameAttempts bounds the <base>-<short>-N collision search.
const maxNameAttempts = 1000

// live is one session the daemon currently tracks in memory.
type live struct {
	s          adapter.Session
	name       string
	conn       ipc.Conn
	mcpPush    bool
	mcpPID     int // the MCP process's parent PID, for proc: placeholders and reverse join
	hookSeen   bool
	pushFailed bool
	busy       bool
	lastHook   time.Time
	statusAt   time.Time
}

var invalidNameChar = regexp.MustCompile(`[^a-z0-9-]`)
var dashRun = regexp.MustCompile(`-+`)

// slugBase turns cwd's base directory name into the first part of a session name.
func slugBase(cwd string) string {
	b := strings.ToLower(filepath.Base(cwd))
	b = invalidNameChar.ReplaceAllString(b, "-")
	b = dashRun.ReplaceAllString(b, "-")
	b = strings.Trim(b, "-")
	if b == "" {
		b = "session"
	}
	return b
}

// buildName joins base and short with an -N suffix for n>1, truncating base (without leaving a trailing '-') so the result stays within 40 chars.
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
		base = strings.TrimRight(base[:max], "-")
	}
	return base + tail
}

// mergeSession merges new into old, keeping old's value for any field new leaves zero/empty (e.g. an MCP re-register carrying no PID or Inbox).
func mergeSession(old, new adapter.Session) adapter.Session {
	if new.Agent != "" {
		old.Agent = new.Agent
	}
	if new.ID != "" {
		old.ID = new.ID
	}
	if new.Cwd != "" {
		old.Cwd = new.Cwd
	}
	if new.PID != 0 {
		old.PID = new.PID
	}
	if new.Transcript != "" {
		old.Transcript = new.Transcript
	}
	if new.Inbox != "" {
		old.Inbox = new.Inbox
	}
	if new.InboxToken != "" {
		old.InboxToken = new.InboxToken
	}
	return old
}

// register handles the "register" op: naming, process-join and the remembered record. Caller must not hold mu.
func (d *Daemon) register(c ipc.Conn, a RegisterArgs) (Registered, error) {
	if !d.opts.FolderAllowed(a.Session.Cwd) {
		return Registered{}, fmt.Errorf("agentnet is off for this folder; run agentnet folder add %s", a.Session.Cwd)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conns[c.ID()] = c

	// Forward join: an MCP connection attaches to the live hook-registered session of the same agent+PID.
	if a.MCPPID != 0 {
		if l := d.findHookSession(a.Session.Agent, a.MCPPID); l != nil {
			l.conn = c
			if a.MCPPush {
				l.mcpPush = true
			}
			return Registered{Key: l.s.Key, Address: d.opts.Person + "/" + l.name, Name: l.name}, nil
		}
	}

	// Reverse join: a hook registration folds in a live MCP-only placeholder for the same process.
	// Only find it here; the fold itself (freeing its name, moving its mail) waits until the
	// incoming key is successfully named, so a naming failure doesn't strand the MCP conn or mail.
	var proc *live
	allowKey := "" // the one placeholder key, if any, the incoming session is about to fold and may reuse the name of
	if a.Hook && a.Session.PID != 0 {
		proc = d.findProcSession(a.Session.Agent, a.Session.PID)
		if proc != nil {
			allowKey = proc.s.Key
		}
	}

	key := a.Session.Key
	l, exists := d.live[key]
	if !exists {
		name, err := d.assignName(key, a.Session.Agent, a.Session.Cwd, allowKey)
		if err != nil {
			return Registered{}, err
		}
		l = &live{s: a.Session, name: name}
		d.live[key] = l
		// A proc: placeholder (no hook has joined it yet) isn't remembered: it's transient, and
		// persisting its name would block the real session from claiming it later.
		if !isPlaceholder(key) {
			rec := store.SessionRecord{Name: name, Key: key, Agent: a.Session.Agent, Cwd: a.Session.Cwd, LastSeen: d.opts.Now()}
			if err := d.opts.Store.UpsertSession(rec); err != nil {
				delete(d.live, key)
				return Registered{}, err
			}
		}
	} else {
		l.s = mergeSession(l.s, a.Session)
	}

	var joinedConn ipc.Conn
	var joinedMCPPush bool
	if proc != nil {
		if err := d.opts.Store.Rehome(proc.s.Key, key); err != nil {
			return Registered{}, err
		}
		joinedConn, joinedMCPPush = proc.conn, proc.mcpPush
		delete(d.live, proc.s.Key)
		d.broadcastArrived() // the placeholder's mail just moved to key; wake anyone long-polling it
	}
	// A hook re-register must not touch conn/mcpPush: only an MCP register (or the reverse join above) owns those.
	if !a.Hook || a.MCPPush || a.MCPPID != 0 {
		l.conn = c
		if a.MCPPush {
			l.mcpPush = true
		}
	}
	if a.MCPPID != 0 {
		l.mcpPID = a.MCPPID
	}
	if joinedConn != nil {
		l.conn = joinedConn // the MCP connection is the pushable one
	}
	if joinedMCPPush {
		l.mcpPush = true
	}
	if a.Hook {
		l.hookSeen = true
		l.lastHook = d.opts.Now()
	}
	// One inbox socket serves one session at a time: a stale pre-/clear hook must not stay armed
	// to push into a socket that now belongs to a different conversation.
	// It self-heals on the live conversation's next hook.
	if a.Hook && a.Session.Inbox != "" {
		for k, other := range d.live {
			if k != key && other.s.Agent == a.Session.Agent && other.s.Inbox == a.Session.Inbox {
				other.s.Inbox = ""
				other.s.InboxToken = ""
			}
		}
	}
	return Registered{Key: key, Address: d.opts.Person + "/" + l.name, Name: l.name}, nil
}

// isPlaceholder reports whether key names a transient MCP-only entry that hasn't joined a hook session yet.
func isPlaceholder(key string) bool { return strings.HasPrefix(key, "proc:") }

// findHookSession returns the hook-registered live session for agent+pid, preferring the most recently active one;
// nil on an exact tie (logged), so the caller falls back to registering the MCP under its own key. Caller holds mu.
func (d *Daemon) findHookSession(agent string, pid int) *live {
	var best *live
	tie := false
	for _, l := range d.live {
		if !l.hookSeen || l.s.Agent != agent || l.s.PID != pid {
			continue
		}
		switch {
		case best == nil || l.lastHook.After(best.lastHook):
			best, tie = l, false
		case l.lastHook.Equal(best.lastHook):
			tie = true
		}
	}
	if tie {
		d.opts.Log("daemon: ambiguous hook session for %s pid %d; registering MCP under its own key", agent, pid)
		return nil
	}
	return best
}

// findProcSession returns the live MCP-only placeholder registered for agent+pid, if any. Caller holds mu.
func (d *Daemon) findProcSession(agent string, pid int) *live {
	for _, l := range d.live {
		if l.mcpPID == pid && l.s.Agent == agent && isPlaceholder(l.s.Key) {
			return l
		}
	}
	return nil
}

// assignName picks the name for key: its remembered name if still free, else the first free <base>-<short>[-N].
// allowKey, if non-empty, is the one placeholder key being folded into key, whose name key may reuse. Caller holds mu.
func (d *Daemon) assignName(key, agentName, cwd, allowKey string) (string, error) {
	rec, err := d.opts.Store.SessionByKey(key)
	if err != nil {
		return "", err
	}
	if rec != nil {
		free, err := d.nameFree(key, rec.Name, allowKey)
		if err != nil {
			return "", err
		}
		if free {
			return rec.Name, nil
		}
	}
	a, ok := d.opts.Adapters.Get(agentName)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	base, short := slugBase(cwd), a.Short()
	for n := 1; n <= maxNameAttempts; n++ {
		name := buildName(base, short, n)
		free, err := d.nameFree(key, name, allowKey)
		if err != nil {
			return "", err
		}
		if free {
			return name, nil
		}
	}
	return "", fmt.Errorf("daemon: no free name for %s after %d attempts", key, maxNameAttempts)
}

// nameFree reports whether name is unused by any live session or remembered record of a different key, other than
// allowKey (the one placeholder being folded into key, whose name key is allowed to take over). Caller holds mu.
func (d *Daemon) nameFree(key, name, allowKey string) (bool, error) {
	for k, l := range d.live {
		if k == key || k == allowKey {
			continue
		}
		if l.name == name {
			return false, nil
		}
	}
	rec, err := d.opts.Store.SessionByName(name)
	if err != nil {
		return false, err
	}
	return rec == nil || rec.Key == key, nil
}

// unregister detaches c from key if it's the session's conn (so a stale push target can't keep
// pushing to it, even if the session's own hooks keep it live), then drops key from live unless
// its hooks ran within hookGrace; the remembered record is untouched.
//
// Detaching clears Inbox/InboxToken too, not just conn/mcpPush: both were supplied by this same
// connection's own registration, and leaving Inbox set would make Channel fall through to
// ChannelCommand and push straight to that socket anyway (e.g. Claude's inbox, now serving a
// different conversation in the same process), defeating the whole point of detaching.
func (d *Daemon) unregister(c ipc.Conn, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.live[key]
	if !ok {
		return
	}
	if l.conn == c {
		l.conn = nil
		l.mcpPush = false
		l.s.Inbox = ""
		l.s.InboxToken = ""
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
