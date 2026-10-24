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

// sessionManager owns the one ipc.Client this MCP server process uses, resolving and
// caching each client session's daemon key, and pushing delivered mail to Claude's inbox.
type sessionManager struct {
	o Options

	mu            sync.Mutex
	client        *ipc.Client
	cache         map[string]string              // "<adapter>:<id>" -> resolved daemon key
	registrations map[string]daemon.RegisterArgs // cache key -> args to replay after a reconnect
}

func newSessionManager(o Options) *sessionManager {
	return &sessionManager{
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

// replay re-sends every registration this process made, best-effort, after a redial.
func (sm *sessionManager) replay(c *ipc.Client) {
	sm.mu.Lock()
	args := make([]daemon.RegisterArgs, 0, len(sm.registrations))
	for _, a := range sm.registrations {
		args = append(args, a)
	}
	sm.mu.Unlock()
	for _, a := range args {
		var reg daemon.Registered
		c.Call(context.Background(), "register", a, &reg)
	}
}

// pushLoop forwards the daemon's pushed "deliver" events to this agent's inbox socket.
func (sm *sessionManager) pushLoop(c *ipc.Client) {
	for ev := range c.Events() {
		sm.handleEvent(c, ev)
	}
}

// handleEvent handles one pushed event, recovering any panic so the loop keeps running.
func (sm *sessionManager) handleEvent(c *ipc.Client, ev ipc.Event) {
	defer func() { recover() }()
	if ev.Event != "deliver" {
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
// with the daemon the first time, and reusing the cached key on later calls.
func (sm *sessionManager) resolveSession(ctx context.Context, req *mcp.CallToolRequest) (string, error) {
	adp := sm.resolveAdapter(req)

	if id := adp.MetaSessionID(req.Params.GetMeta()); id != "" {
		return sm.registerCached(ctx, adp, id)
	}
	if adp.Multiplexed() {
		return sm.attach(ctx, adp)
	}
	id := adp.EnvSessionID(sm.o.Getenv)
	if id == "" && adp.Name() == "claude-code" {
		if sid, ok := claude.SessionFromRegistry(sm.o.ClaudeDir, sm.o.ParentPID); ok {
			id = sid
		}
	}
	if id == "" {
		id = fmt.Sprintf("proc:%d", sm.o.ParentPID)
	}
	return sm.registerCached(ctx, adp, id)
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

// registerCached resolves (adp, id) to a daemon key, calling "register" once and caching the result.
func (sm *sessionManager) registerCached(ctx context.Context, adp adapter.Adapter, id string) (string, error) {
	cacheKey := adp.Name() + ":" + id
	sm.mu.Lock()
	if key, ok := sm.cache[cacheKey]; ok {
		sm.mu.Unlock()
		return key, nil
	}
	sm.mu.Unlock()

	sess := adapter.Session{Key: cacheKey, Agent: adp.Name(), ID: id, Cwd: sm.o.Cwd, PID: sm.o.ParentPID}
	mcpPush := false
	if adp.Name() == "claude-code" {
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
