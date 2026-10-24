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
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

// testEnv is a running test daemon plus the policy a test can flip (env.allowed).
type testEnv struct {
	sock    string
	allowed func(string) bool
}

// startDaemon runs a real daemon behind a short Unix socket for the life of the test.
func startDaemon(t *testing.T) *testEnv {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	env := &testEnv{allowed: func(string) bool { return true }}
	d := daemon.New(daemon.Options{
		Person:  "alex",
		Machine: "mbp",
		Store:   storetest.SQLite(t),
		// claude.New() is here too (beyond the brief's bare generic registry) so a
		// claude-code session can be registered and pushed to in the push-loop test.
		Adapters:      adapter.NewRegistry(generic.WithEnv("mcp", "AGENTNET_TEST_SESSION"), claude.New()),
		FolderAllowed: func(dir string) bool { return env.allowed(dir) },
		Now:           time.Now,
		Log:           t.Logf,
	})

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
