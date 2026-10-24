package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/message"
)

// HookArgs is the "hook" op's arguments: one call from an agent's hook command.
type HookArgs struct {
	Agent      string            `json:"agent"`
	Event      adapter.HookEvent `json:"event"`
	SessionID  string            `json:"session_id"`
	Cwd        string            `json:"cwd"`
	PID        int               `json:"pid,omitempty"`
	Transcript string            `json:"transcript,omitempty"`
	Inbox      string            `json:"inbox,omitempty"`
	InboxToken string            `json:"inbox_token,omitempty"`
}

// HookResult answers a "hook" op: text to print (if any), the session's key and the ids it covers.
type HookResult struct {
	Out string   `json:"out"`
	Key string   `json:"key"`
	IDs []string `json:"ids"`
}

// hook handles the "hook" op: registers the session, updates its busy/idle status, and returns any pending mail.
func (d *Daemon) hook(c ipc.Conn, a HookArgs) (HookResult, error) {
	if !d.opts.FolderAllowed(a.Cwd) {
		return HookResult{}, nil
	}
	reg, err := d.register(c, RegisterArgs{
		Session: adapter.Session{
			Key: a.Agent + ":" + a.SessionID, Agent: a.Agent, ID: a.SessionID, Cwd: a.Cwd,
			PID: a.PID, Transcript: a.Transcript, Inbox: a.Inbox, InboxToken: a.InboxToken,
		},
		Hook: true,
	})
	if err != nil {
		return HookResult{}, err
	}
	key := reg.Key

	busy := a.Event == adapter.EventPrompt || a.Event == adapter.EventTool
	d.mu.Lock()
	l, ok := d.live[key]
	if ok {
		l.busy = busy
		l.statusAt = d.opts.Now()
	}
	if a.Event == adapter.EventEnd && ok && l.conn == nil {
		delete(d.live, key)
	}
	d.mu.Unlock()

	msgs, err := d.pending(key)
	if err != nil {
		return HookResult{}, err
	}
	if a.Event == adapter.EventTool {
		msgs = wakeOnly(msgs)
	}
	if len(msgs) == 0 {
		return HookResult{Key: key}, nil
	}

	ids := idsOf(msgs)
	d.mu.Lock()
	now := d.opts.Now()
	for _, id := range ids {
		d.inFlight[id] = now
	}
	d.mu.Unlock()

	adp, ok := d.opts.Adapters.Get(a.Agent)
	if !ok {
		return HookResult{Key: key, IDs: ids}, nil
	}
	out, ok := adp.HookOutput(a.Event, message.ForAgent(msgs))
	if !ok {
		return HookResult{Key: key, IDs: ids}, nil
	}
	return HookResult{Out: out, Key: key, IDs: ids}, nil
}

// AttachArgs is the "attach" op's arguments: a multiplexed agent's one MCP process finding its live session.
type AttachArgs struct {
	Agent string `json:"agent"`
	Cwd   string `json:"cwd"`
}

// attach handles the "attach" op: the caller's conn becomes the pushable one for the sole busy, hook-seen session it matches.
func (d *Daemon) attach(c ipc.Conn, a AttachArgs) (Registered, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.soleBusyHookSeen(a.Agent, a.Cwd)
	if l == nil {
		l = d.soleBusyHookSeen(a.Agent, "")
	}
	if l == nil {
		return Registered{}, fmt.Errorf("no single busy %s session to attach to", a.Agent)
	}
	l.conn = c
	l.mcpPush = true
	return Registered{Key: l.s.Key, Address: d.opts.Person + "/" + l.name, Name: l.name}, nil
}

// soleBusyHookSeen returns the one busy, hook-seen live session for agent (and cwd, if cwd != ""), or nil if there isn't exactly one. Caller holds mu.
func (d *Daemon) soleBusyHookSeen(agent, cwd string) *live {
	var found *live
	for _, l := range d.live {
		if l.s.Agent != agent || !l.hookSeen || !l.busy {
			continue
		}
		if cwd != "" && l.s.Cwd != cwd {
			continue
		}
		if found != nil {
			return nil // ambiguous
		}
		found = l
	}
	return found
}

// WhoArgs is the "who" op's arguments: the caller's own key, to mark its entry.
type WhoArgs struct {
	Key string `json:"key"`
}

// WhoResult answers a "who" op.
type WhoResult struct {
	Sessions []WhoEntry `json:"sessions"`
}

// WhoEntry is one live session as "who" reports it.
type WhoEntry struct {
	Address string       `json:"address"`
	Agent   string       `json:"agent"`
	Repo    string       `json:"repo,omitempty"`
	Branch  string       `json:"branch,omitempty"`
	Status  string       `json:"status"` // busy | idle | offline
	Tier    adapter.Tier `json:"tier"`
	You     bool         `json:"you"`
}

// whoSnap is one live session's fields, copied out while holding mu so git lookups and adapter calls don't need the lock.
type whoSnap struct {
	key, name string
	s         adapter.Session
	busy      bool
	hasConn   bool
	st        adapter.State
}

// who handles the "who" op: live sessions here, with git info, status and tier.
func (d *Daemon) who(a WhoArgs) WhoResult {
	d.mu.Lock()
	snaps := make([]whoSnap, 0, len(d.live))
	for key, l := range d.live {
		if isPlaceholder(key) {
			continue
		}
		snaps = append(snaps, whoSnap{
			key: key, name: l.name, s: l.s, busy: l.busy, hasConn: l.conn != nil,
			st: adapter.State{MCPPush: l.mcpPush, HookSeen: l.hookSeen, PushFailed: l.pushFailed},
		})
	}
	d.mu.Unlock()

	now := d.opts.Now()
	var res WhoResult
	for _, sn := range snaps {
		repo, branch := d.gitInfoFor(sn.s.Cwd, now)
		status := "idle"
		switch {
		case sn.busy:
			status = "busy"
		case !sn.hasConn && !sn.st.MCPPush:
			status = "offline"
		}
		var tier adapter.Tier
		if adp, ok := d.opts.Adapters.Get(sn.s.Agent); ok {
			tier = adp.Tier(sn.s, sn.st)
		}
		res.Sessions = append(res.Sessions, WhoEntry{
			Address: d.opts.Person + "/" + sn.name, Agent: sn.s.Agent, Repo: repo, Branch: branch,
			Status: status, Tier: tier, You: sn.key == a.Key,
		})
	}
	return res
}

// gitInfo is one cwd's cached repo/branch.
type gitInfo struct {
	repo, branch string
	at           time.Time
}

const gitInfoTTL = 30 * time.Second
const gitTimeout = 2 * time.Second

// gitInfoFor returns cwd's branch and remote-origin repo basename, cached for gitInfoTTL; errors leave both empty. Never holds d.mu.
func (d *Daemon) gitInfoFor(cwd string, now time.Time) (repo, branch string) {
	d.gitMu.Lock()
	if c, ok := d.gitCache[cwd]; ok && now.Sub(c.at) < gitInfoTTL {
		d.gitMu.Unlock()
		return c.repo, c.branch
	}
	d.gitMu.Unlock()

	branch = gitOutput(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	repo = repoBasename(gitOutput(cwd, "remote", "get-url", "origin"))

	d.gitMu.Lock()
	d.gitCache[cwd] = gitInfo{repo: repo, branch: branch, at: now}
	d.gitMu.Unlock()
	return repo, branch
}

// gitOutput runs git -C cwd args... with a short timeout, returning "" on any error.
func gitOutput(cwd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// repoBasename extracts the repo name from a remote URL, dropping a trailing ".git".
func repoBasename(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	return strings.TrimSuffix(path.Base(remoteURL), ".git")
}
