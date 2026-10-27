package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/opencode"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// TestOpencodeClientNameAndMetaResolve pins the live OpenCode 2.x contract: its MCP client
// declares itself as "opencode" (matched by the adapter's Name, no ClientNames alias), and every
// tool call carries _meta["ai.opencode/sessionID"], which keys the real session — not a
// proc:<pid> placeholder.
func TestOpencodeClientNameAndMetaResolve(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	opts := Options{
		Dial:      func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters:  adapter.NewRegistry(opencode.New()),
		Getenv:    func(string) string { return "" },
		Cwd:       "/src/oc",
		ParentPID: 9003,
	}

	t1, t2 := mcp.NewInMemoryTransports()
	go Run(ctx, t1, opts)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "opencode", Version: "2.0.25"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	meta := mcp.Meta{"ai.opencode/sessionID": "ses_1"}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_team", Meta: meta})
	if err != nil || res.IsError {
		t.Fatalf("list_team: %v %+v text=%q", err, res, text(res))
	}
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_team", Meta: meta})
	if err != nil || res2.IsError {
		t.Fatalf("list_team #2: %v %+v", err, res2)
	}
	if text(res) != text(res2) {
		t.Fatalf("expected a stable session across calls: first=%q second=%q", text(res), text(res2))
	}

	client, err := ipc.Dial(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var who daemon.WhoResult
	if err := client.Call(ctx, "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, s := range who.Sessions {
		addresses = append(addresses, s.Address)
	}
	if !strings.Contains(strings.Join(addresses, ","), "-opencode") {
		t.Fatalf("expected the meta session to be registered under an opencode name, got %v", addresses)
	}
}

// TestOpencodeTwoSessionsSharingOnePIDRegression pins the review's crash: session A reports state
// through the plugin (one shared server PID), then session B's first MCP call with B's own meta
// key must register B under B — never fold B into A, so B can't read or send as A.
func TestOpencodeTwoSessionsSharingOnePIDRegression(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	dial, err := ipc.Dial(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer dial.Close()

	// A lives first, through the plugin's status report (`agentnet hook opencode status --state busy`).
	var hookRes daemon.HookResult
	if err := dial.Call(ctx, "hook", daemon.HookArgs{Agent: "opencode", Event: adapter.EventStatus, SessionID: "ses_a", Cwd: "/src/oc", PID: 77, State: "busy"}, &hookRes); err != nil {
		t.Fatal(err)
	}

	// B arrives: same OpenCode server PID, its own meta id.
	opts := Options{
		Dial:      func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters:  adapter.NewRegistry(opencode.New()),
		Getenv:    func(string) string { return "" },
		Cwd:       "/src/oc",
		ParentPID: 77,
	}
	t1, t2 := mcp.NewInMemoryTransports()
	go Run(ctx, t1, opts)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "opencode", Version: "2.0.25"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_team", Meta: mcp.Meta{"ai.opencode/sessionID": "ses_b"}})
	if err != nil || res.IsError {
		t.Fatalf("list_team: %v %+v text=%q", err, res, text(res))
	}

	var who daemon.WhoResult
	if err := dial.Call(ctx, "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, s := range who.Sessions {
		if strings.Contains(s.Address, "-opencode") {
			addresses = append(addresses, s.Address)
		}
	}
	if len(addresses) != 2 {
		t.Fatalf("expected two distinct opencode sessions (A and B), got %d: %v", len(addresses), addresses)
	}
}
