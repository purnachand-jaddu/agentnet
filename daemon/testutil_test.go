package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

type fakeAdapter struct {
	name, short string
	channel     adapter.Channel
	clientNames []string
	mu          sync.Mutex
	pushed      []string
	running     bool
	failPush    bool // Push returns an error instead of recording text
}

func (f *fakeAdapter) ClientNames() []string { return f.clientNames }

func (f *fakeAdapter) Name() string                            { return f.name }
func (f *fakeAdapter) Short() string                           { return f.short }
func (f *fakeAdapter) EnvSessionID(func(string) string) string { return "" }
func (f *fakeAdapter) MetaSessionID(map[string]any) string     { return "" }
func (f *fakeAdapter) Multiplexed() bool                       { return false }

// Channel returns ChannelHook once st.PushFailed is set, so tests can observe the daemon feeding it back in.
func (f *fakeAdapter) Channel(_ adapter.Session, st adapter.State) adapter.Channel {
	if st.PushFailed {
		return adapter.ChannelHook
	}
	return f.channel
}
func (f *fakeAdapter) Push(_ context.Context, _ adapter.Session, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPush {
		return errors.New("push failed")
	}
	f.pushed = append(f.pushed, text)
	return nil
}
func (f *fakeAdapter) HookOutput(_ adapter.HookEvent, text string) (string, bool) {
	return "HOOK:" + text, true
}
func (f *fakeAdapter) Tier(adapter.Session, adapter.State) adapter.Tier { return adapter.Push }
func (f *fakeAdapter) Pushed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushed...)
}

type fakeConn struct {
	id       uint64
	mu       sync.Mutex
	events   []ipc.Event
	failPush bool // Push returns an error instead of recording the event
}

func (c *fakeConn) ID() uint64 { return c.id }
func (c *fakeConn) Push(ev ipc.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failPush {
		return errors.New("conn push failed")
	}
	c.events = append(c.events, ev)
	return nil
}
func (c *fakeConn) Events() []ipc.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ipc.Event(nil), c.events...)
}

func codexFake(ch adapter.Channel) *fakeAdapter {
	return &fakeAdapter{name: "codex", short: "codex", channel: ch}
}
func claudeFake(ch adapter.Channel) *fakeAdapter {
	return &fakeAdapter{name: "claude-code", short: "claude", channel: ch}
}

func newTestDaemon(t *testing.T, as ...adapter.Adapter) *Daemon {
	return New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(as...),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf})
}

func call[T any](t *testing.T, d *Daemon, c ipc.Conn, op string, args any) T {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := d.Handle(context.Background(), c, op, raw)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var out T
	b, _ := json.Marshal(res)
	json.Unmarshal(b, &out)
	return out
}

func register(t *testing.T, d *Daemon, c ipc.Conn, agent, id, cwd string, push bool) Registered {
	t.Helper()
	return call[Registered](t, d, c, "register", RegisterArgs{Session: adapter.Session{Key: agent + ":" + id, Agent: agent, ID: id, Cwd: cwd}, MCPPush: push})
}

// init shortens the turn-watch poll so tests don't wait on the production 2s interval.
func init() {
	turnWatchInterval = 20 * time.Millisecond
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
