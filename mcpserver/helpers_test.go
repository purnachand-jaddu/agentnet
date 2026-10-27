package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/claude"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
	"github.com/purnachand-jaddu/agentnet/adapter/opencode"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

// testEnv is a running test daemon plus the policy a test can flip (env.allowed).
type testEnv struct {
	sock    string
	allowed func(string) bool
}

// testAdapters is the daemon-side adapter registry shared by every test in this package.
// Beyond the plain generic adapter, it carries claude.New() (so a claude-code session can be
// registered and pushed to) and the two fake adapters below (so assignName can name their
// placeholder/attached sessions): none of this affects the plain generic-adapter tests, since
// none of them set CLAUDE_CODE_SESSION_ID or declare an MCP client name those fakes claim.
func testAdapters() *adapter.Registry {
	return adapter.NewRegistry(
		generic.WithEnv("mcp", "AGENTNET_TEST_SESSION"),
		claude.New(),
		opencode.New(),
		newFakeMultiplexedAdapter(),
		newFakeMetaAdapter(),
	)
}

// newTestDaemon builds a daemon.Daemon with a fresh in-memory store and the shared test registry.
func newTestDaemon(t *testing.T, allowed func(string) bool) *daemon.Daemon {
	return daemon.New(daemon.Options{
		Person:        "alex",
		Machine:       "mbp",
		Store:         storetest.SQLite(t),
		Adapters:      testAdapters(),
		FolderAllowed: allowed,
		Now:           time.Now,
		Log:           t.Logf,
	})
}

// startDaemon runs a real daemon behind a short Unix socket for the life of the test.
func startDaemon(t *testing.T) *testEnv {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	env := &testEnv{allowed: func(string) bool { return true }}
	d := newTestDaemon(t, func(dir string) bool { return env.allowed(dir) })

	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	env.sock = filepath.Join(dir, "s.sock")

	l, err := ipc.Listen(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	go ipc.Serve(ctx, l, d.Handle, d.ConnClosed)
	return env
}

// options builds mcpserver.Options for a generic-adapter session with session id and cwd.
func (env *testEnv) options(id, cwd string) Options {
	return Options{
		Dial:     func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters: adapter.NewRegistry(generic.WithEnv("mcp", "AGENTNET_TEST_SESSION")),
		Getenv: func(k string) string {
			if k == "AGENTNET_TEST_SESSION" {
				return id
			}
			return ""
		},
		Cwd: cwd,
	}
}

// connect runs Run on one end of an in-memory transport pair and returns the connected client session.
func connect(t *testing.T, ctx context.Context, o Options) *mcp.ClientSession {
	t1, t2 := mcp.NewInMemoryTransports()
	go Run(ctx, t1, o)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// fakeMultiplexedAdapter is a minimal multiplexed test adapter (no push capability, pull tier),
// selected via FromClient because connect's client always declares itself as "test-client".
type fakeMultiplexedAdapter struct{ *generic.Adapter }

func newFakeMultiplexedAdapter() *fakeMultiplexedAdapter {
	return &fakeMultiplexedAdapter{Adapter: generic.WithEnv("fakemux", "")}
}
func (f *fakeMultiplexedAdapter) Multiplexed() bool     { return true }
func (f *fakeMultiplexedAdapter) ClientNames() []string { return []string{"test-client"} }

// fakeMetaAdapter is a multiplexed test adapter whose session id always comes from the call's
// _meta, to prove meta-id resolution takes priority over an attach call.
type fakeMetaAdapter struct{ *generic.Adapter }

func newFakeMetaAdapter() *fakeMetaAdapter {
	return &fakeMetaAdapter{Adapter: generic.WithEnv("fakemeta", "")}
}
func (f *fakeMetaAdapter) Multiplexed() bool     { return true }
func (f *fakeMetaAdapter) ClientNames() []string { return []string{"test-client"} }
func (f *fakeMetaAdapter) MetaSessionID(meta map[string]any) string {
	if v, ok := meta["session_id"].(string); ok {
		return v
	}
	return ""
}
