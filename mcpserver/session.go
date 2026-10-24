package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/claude"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// pushPostTimeout bounds posting a pushed message to the local agent's inbox socket.
const pushPostTimeout = 10 * time.Second

// claudeAdapterName is the claude-code adapter's Name(), checked without importing a concrete type.
const claudeAdapterName = "claude-code"

// reconnectMinBackoff and reconnectMaxBackoff bound the proactive-redial backoff.
const (
	reconnectMinBackoff = 500 * time.Millisecond
	reconnectMaxBackoff = 10 * time.Second
)

// placeholderID is the fallback id/key for a session the daemon can't otherwise name: it must
// be exactly "proc:<pid>" because the daemon recognises a placeholder only by that key prefix
// (daemon.isPlaceholder, findProcSession), to fold into the hook's own registration later.
func placeholderID(pid int) string { return fmt.Sprintf("proc:%d", pid) }

// sessionManager owns the one ipc.Client this MCP server process uses, resolving and
// caching each client session's daemon key, and pushing delivered mail to Claude's inbox.
type sessionManager struct {
	o   Options
	ctx context.Context // Run's ctx; stops the proactive reconnect loop once it's done

	mu            sync.Mutex
	client        *ipc.Client
	cache         map[string]string              // "<adapter>:<id>" (or a bare "proc:<pid>") -> resolved daemon key
	registrations map[string]daemon.RegisterArgs // same cache key -> args to replay after a reconnect
	claudeID      string                         // the last Claude session id resolved, re-read every call
	servedKey     string                         // the key the most recent tool call resolved to; guards which pushes handleEvent will post
}

func newSessionManager(ctx context.Context, o Options) *sessionManager {
	if o.Logger == nil {
		o.Logger = defaultLogger()
	}
	return &sessionManager{
		ctx:           ctx,
		o:             o,
		cache:         map[string]string{},
		registrations: map[string]daemon.RegisterArgs{},
	}
}

// conn returns the current ipc.Client, dialing lazily and starting its push loop.
func (sm *sessionManager) conn() (*ipc.Client, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.client != nil {
		return sm.client, nil
	}
	c, err := sm.o.Dial()
	if err != nil {
		return nil, err
	}
	sm.client = c
	go sm.pushLoop(c)
	return c, nil
}

// call runs op on the daemon, redialing once and replaying this process's registrations
// if the call failed because the connection had died.
func (sm *sessionManager) call(ctx context.Context, op string, args, result any) error {
	c, err := sm.conn()
	if err != nil {
		return err
	}
	err = c.Call(ctx, op, args, result)
	if err == nil || err.Error() != "ipc: connection closed" {
		return err
	}

	sm.mu.Lock()
	if sm.client == c {
		sm.client = nil
	}
	sm.mu.Unlock()
	c2, derr := sm.conn()
	if derr != nil {
		return err
	}
	sm.replay(c2)
	return c2.Call(ctx, op, args, result)
}

// replay re-sends every registration this process made, best-effort, after a redial, updating
// the cache from each call's own Registered.Key (the daemon may assign a different one).
func (sm *sessionManager) replay(c *ipc.Client) {
	sm.mu.Lock()
	entries := make(map[string]daemon.RegisterArgs, len(sm.registrations))
	for k, a := range sm.registrations {
		entries[k] = a
	}
	sm.mu.Unlock()
	for cacheKey, a := range entries {
		var reg daemon.Registered
		if err := c.Call(context.Background(), "register", a, &reg); err != nil {
			continue
		}
		sm.mu.Lock()
		sm.cache[cacheKey] = reg.Key
		sm.mu.Unlock()
	}
}

// pushLoop forwards the daemon's pushed "deliver" events to this agent's inbox socket. When the
// connection dies on its own (e.g. a daemon restart), it reconnects proactively rather than
// waiting for the next tool call to notice.
func (sm *sessionManager) pushLoop(c *ipc.Client) {
	for ev := range c.Events() {
		sm.handleEvent(c, ev)
	}
	sm.mu.Lock()
	if sm.client == c {
		sm.client = nil
	}
	sm.mu.Unlock()
	if sm.ctx.Err() == nil {
		sm.reconnectLoop()
	}
}

// reconnectLoop redials with exponential backoff (0.5s doubling to a 10s cap) until conn()
// succeeds, then replays this process's registrations on the new connection and resumes the
// push loop. It routes every dial through conn() so it can't race a tool call's own redial
// into opening two connections at once.
func (sm *sessionManager) reconnectLoop() {
	backoff := reconnectMinBackoff
	for {
		if sm.ctx.Err() != nil {
			return
		}
		if c, err := sm.conn(); err == nil {
			sm.replay(c)
			return
		}
		select {
		case <-time.After(backoff):
		case <-sm.ctx.Done():
			return
		}
		if backoff < reconnectMaxBackoff {
			backoff *= 2
			if backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
		}
	}
}

// handleEvent handles one pushed event, recovering any panic so the loop keeps running.
// Defence in depth: it only ever posts for the key this process currently serves (the most
// recent tool call's resolved session). A push for any other key means this process is no
// longer the right target for it — most likely a stale push that arrived for a session this
// process has since moved on from (e.g. after /clear) — so it's reported back as failed
// instead of landing in the wrong conversation's inbox.
func (sm *sessionManager) handleEvent(c *ipc.Client, ev ipc.Event) {
	defer func() { recover() }()
	if ev.Event != "deliver" {
		return
	}
	sm.mu.Lock()
	served := sm.servedKey
	sm.mu.Unlock()
	if served != "" && ev.Key != served {
		c.Call(context.Background(), "push_failed", daemon.PushFailedArgs{Key: ev.Key, IDs: ev.IDs}, nil)
		return
	}

	socket, token := claude.InboxFromEnv(sm.o.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), pushPostTimeout)
	defer cancel()
	if err := claude.PostInboxContext(ctx, socket, token, ev.Text); err != nil {
		c.Call(context.Background(), "push_failed", daemon.PushFailedArgs{Key: ev.Key, IDs: ev.IDs}, nil)
		return
	}
	c.Call(context.Background(), "delivered", daemon.DeliveredArgs{Key: ev.Key, IDs: ev.IDs}, nil)
}

// resolveSession returns the daemon key for this call's session, registering (or attaching)
// with the daemon as needed, and reusing a cached key where that's safe. It records the
// resolved key as the one this process currently serves, for handleEvent's guard.
func (sm *sessionManager) resolveSession(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	key, err := sm.resolveSessionKey(ctx, req)
	if err != nil {
		return "", err
	}
	sm.mu.Lock()
	sm.servedKey = key
	sm.mu.Unlock()
	return key, nil
}

// resolveSessionKey is resolveSession's body, kept separate so resolveSession has one place to
// record the result as the currently served key.
func (sm *sessionManager) resolveSessionKey(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	adp := sm.resolveAdapter(req)

	if id := adp.MetaSessionID(req.Params.GetMeta()); id != "" {
		return sm.registerCached(ctx, adp, id, false)
	}
	if adp.Multiplexed() {
		if key, err := sm.attach(ctx, adp); err == nil {
			return key, nil
		}
		// No sole busy session to attach to yet: fall back to a pull-tier placeholder
		// rather than erroring on every call until one shows up.
		return sm.registerCached(ctx, adp, placeholderID(sm.o.ParentPID), true)
	}
	if adp.Name() == claudeAdapterName {
		return sm.resolveClaudeSession(ctx, adp)
	}
	if id := adp.EnvSessionID(sm.o.Getenv); id != "" {
		return sm.registerCached(ctx, adp, id, false)
	}
	return sm.registerCached(ctx, adp, placeholderID(sm.o.ParentPID), true)
}

// resolveClaudeSession re-resolves Claude's session id on every call (registry first, env
// fallback) instead of caching it for the process lifetime: after /clear, the MCP process's
// env still names the old conversation while the hook registers the new one under the same
// PID. When the id has changed since the last call, it drops the stale registration before
// registering the new one, so the new session can reclaim the old one's name.
func (sm *sessionManager) resolveClaudeSession(ctx context.Context, adp adapter.Adapter) (string, error) {
	id, ok := claude.SessionFromRegistry(sm.o.ClaudeDir, sm.o.ParentPID)
	if !ok {
		id = adp.EnvSessionID(sm.o.Getenv)
	}
	if id == "" {
		return sm.registerCached(ctx, adp, placeholderID(sm.o.ParentPID), true)
	}

	sm.mu.Lock()
	prevID := sm.claudeID
	sm.mu.Unlock()
	if prevID != "" && prevID != id {
		sm.dropRegistration(ctx, adp, prevID)
	}
	sm.mu.Lock()
	sm.claudeID = id
	sm.mu.Unlock()
	return sm.registerCached(ctx, adp, id, false)
}

// dropRegistration unregisters and forgets a previously cached (adp, id) session.
func (sm *sessionManager) dropRegistration(ctx context.Context, adp adapter.Adapter, id string) {
	cacheKey := adp.Name() + ":" + id
	sm.mu.Lock()
	key := sm.cache[cacheKey]
	delete(sm.cache, cacheKey)
	delete(sm.registrations, cacheKey)
	sm.mu.Unlock()
	if key != "" {
		sm.call(ctx, "unregister", daemon.UnregisterArgs{Key: key}, nil)
	}
}

// resolveAdapter picks the adapter for req: by MCP client name, else by environment, else generic.
func (sm *sessionManager) resolveAdapter(req *mcp.CallToolRequest) adapter.Adapter {
	if ss := req.Session; ss != nil {
		if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			if adp, ok := sm.o.Adapters.FromClient(ip.ClientInfo.Name); ok {
				return adp
			}
		}
	}
	if adp, ok := sm.o.Adapters.FromEnv(sm.o.Getenv); ok {
		return adp
	}
	return generic.New()
}

// registerCached resolves (adp, id) to a daemon key, calling "register" once and caching the
// result. placeholder keys a transient, unnamed session as exactly id (which must already be
// "proc:<pid>"); anything else is cached and keyed as "<adapter>:<id>".
func (sm *sessionManager) registerCached(ctx context.Context, adp adapter.Adapter, id string, placeholder bool) (string, error) {
	cacheKey, key := adp.Name()+":"+id, adp.Name()+":"+id
	if placeholder {
		cacheKey, key = id, id
	}

	sm.mu.Lock()
	if k, ok := sm.cache[cacheKey]; ok {
		sm.mu.Unlock()
		return k, nil
	}
	sm.mu.Unlock()

	sess := adapter.Session{Key: key, Agent: adp.Name(), ID: id, Cwd: sm.o.Cwd, PID: sm.o.ParentPID}
	mcpPush := false
	if adp.Name() == claudeAdapterName {
		if socket, token := claude.InboxFromEnv(sm.o.Getenv); socket != "" {
			sess.Inbox, sess.InboxToken = socket, token
			mcpPush = true
		}
	}
	args := daemon.RegisterArgs{Session: sess, MCPPID: sm.o.ParentPID, MCPPush: mcpPush}

	var reg daemon.Registered
	if err := sm.call(ctx, "register", args, &reg); err != nil {
		return "", err
	}

	sm.mu.Lock()
	sm.cache[cacheKey] = reg.Key
	sm.registrations[cacheKey] = args
	sm.mu.Unlock()
	return reg.Key, nil
}

// attach resolves a multiplexed adapter's sole busy session via the "attach" op. Not cached:
// which session is busy can change between calls.
func (sm *sessionManager) attach(ctx context.Context, adp adapter.Adapter) (string, error) {
	var reg daemon.Registered
	err := sm.call(ctx, "attach", daemon.AttachArgs{Agent: adp.Name(), Cwd: sm.o.Cwd, MCPPush: false}, &reg)
	if err != nil {
		return "", err
	}
	return reg.Key, nil
}
