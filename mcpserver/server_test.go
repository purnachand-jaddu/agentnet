package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/claude"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

func text(r *mcp.CallToolResult) string { return r.Content[0].(*mcp.TextContent).Text }

// startFakeInbox runs a fake Claude inbox listener on a short Unix socket, returning its path
// and a channel of every line (frame) it receives.
func startFakeInbox(t *testing.T) (sock string, lines chan string) {
	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "inbox.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	lines = make(chan string, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}(c)
		}
	}()
	return sock, lines
}

// waitForUserFrame blocks until lines produces a `"type":"user"` frame, or fails the test.
func waitForUserFrame(t *testing.T, lines chan string, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, `"type":"user"`) {
				return
			}
		case <-deadline:
			t.Fatal("inbox listener never received a user frame")
		}
	}
}

func TestSendAndReceiveBetweenTwoMCPSessions(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	a := connect(t, ctx, env.options("A", "/src/a"))
	b := connect(t, ctx, env.options("B", "/src/b"))
	if _, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/b-mcp", "message": "hello b"}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}
	got, _ := b.CallTool(ctx, &mcp.CallToolParams{Name: "check_inbox", Arguments: map[string]any{"wait_secs": 2}})
	if s := text(got); !strings.Contains(s, "hello b") || !strings.Contains(s, "not your user") {
		t.Fatalf("inbox = %q", s)
	}
}

func TestUnreadNoticeOnOtherTools(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	a := connect(t, ctx, env.options("A", "/src/a"))
	b := connect(t, ctx, env.options("B", "/src/b"))
	b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"})
	a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/b-mcp", "message": "x"}})
	got, _ := b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"})
	if !strings.Contains(text(got), "1 unread message") {
		t.Fatalf("no unread notice: %q", text(got))
	}
}

func TestOffFolderRefusesTools(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	env.allowed = func(string) bool { return false }
	c := connect(t, ctx, env.options("A", "/private/thing"))
	res, _ := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"})
	if !res.IsError || !strings.Contains(text(res), "off for this folder") {
		t.Fatalf("got %+v", res)
	}
}

func TestSendToUnknownSessionIsToolError(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	a := connect(t, ctx, env.options("A", "/src/a"))
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/nope", "message": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("want a tool error, got %+v", res)
	}
}

// TestPushToClaudeInbox exercises the push loop: a claude-code session is pushed to over its
// own inbox socket, and the message is marked read without ever calling check_inbox.
func TestPushToClaudeInbox(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	inboxSock, lines := startFakeInbox(t)

	bOpts := Options{
		Dial:     func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters: adapter.NewRegistry(claude.New(), generic.WithEnv("mcp", "AGENTNET_TEST_SESSION")),
		Getenv: func(k string) string {
			switch k {
			case "CLAUDE_CODE_SESSION_ID":
				return "B"
			case "CLAUDE_CODE_MESSAGING_SOCKET":
				return inboxSock
			}
			return ""
		},
		Cwd: "/src/b",
	}
	b := connect(t, ctx, bOpts)
	if _, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	a := connect(t, ctx, env.options("A", "/src/a"))
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/b-claude", "message": "hi push"}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}

	waitForUserFrame(t, lines, 5*time.Second)

	deadline := time.After(5 * time.Second)
	for {
		got, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text(got), "unread message") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("message still unread: %q", text(got))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestProcPlaceholderReverseJoinsWithHook: a generic client with no env id registers under a
// bare "proc:<ppid>" key (not "<adapter>:proc:<ppid>" — the daemon only recognises a
// placeholder by that exact key prefix). A hook register with the same PID must then fold it
// into one session instead of leaving two live entries behind.
func TestProcPlaceholderReverseJoinsWithHook(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	const ppid = 4242

	opts := Options{
		Dial:      func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters:  adapter.NewRegistry(generic.WithEnv("mcp", "AGENTNET_TEST_SESSION")),
		Getenv:    func(string) string { return "" },
		Cwd:       "/src/a",
		ParentPID: ppid,
	}
	a := connect(t, ctx, opts)
	if _, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	c, err := ipc.Dial(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var hr daemon.HookResult
	err = c.Call(ctx, "hook", daemon.HookArgs{Agent: "mcp", Event: adapter.EventStart, SessionID: "s1", Cwd: "/src/a", PID: ppid}, &hr)
	if err != nil {
		t.Fatal(err)
	}

	var who daemon.WhoResult
	if err := c.Call(ctx, "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatal(err)
	}
	if len(who.Sessions) != 1 {
		t.Fatalf("want 1 joined session, got %d: %+v", len(who.Sessions), who.Sessions)
	}
}

// TestClaudeSessionReResolvesAfterRegistryChange: Claude's session id is re-read from the
// registry file every call, not cached for the process lifetime. When it changes (as after
// /clear), the new call must register the new id and drop the stale one.
func TestClaudeSessionReResolvesAfterRegistryChange(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)

	dir := t.TempDir()
	const ppid = 5151
	writeRegistry := func(sessionID string) {
		b, _ := json.Marshal(map[string]any{"sessionId": sessionID})
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", ppid)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRegistry("old")

	opts := Options{
		Dial:     func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters: adapter.NewRegistry(claude.New()),
		// A nonzero CLAUDE_CODE_SESSION_ID only picks the claude-code adapter (via FromEnv);
		// the registry file above always wins for the actual id, per resolveClaudeSession.
		Getenv: func(k string) string {
			if k == "CLAUDE_CODE_SESSION_ID" {
				return "env-placeholder"
			}
			return ""
		},
		Cwd:       "/src/c",
		ParentPID: ppid,
		ClaudeDir: dir,
	}
	c := connect(t, ctx, opts)
	if _, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	writeRegistry("new")

	a := connect(t, ctx, env.options("A", "/src/a"))
	if _, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}
	sres, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/a-mcp", "message": "hi after clear"}})
	if err != nil || sres.IsError {
		t.Fatalf("send: %v %+v", err, sres)
	}

	got, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "check_inbox", Arguments: map[string]any{"wait_secs": 2}})
	if err != nil {
		t.Fatal(err)
	}
	// the new claude session reclaims the "c-claude" name freed by unregistering the old one.
	if !strings.Contains(text(got), "alex/c-claude") {
		t.Fatalf("expected send from the re-resolved session, got %q", text(got))
	}

	var who daemon.WhoResult
	raw, err := ipc.Dial(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.Call(ctx, "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatal(err)
	}
	claudeSessions := 0
	for _, s := range who.Sessions {
		if s.Agent == "claude-code" {
			claudeSessions++
		}
	}
	if claudeSessions != 1 {
		t.Fatalf("want exactly 1 live claude-code session after re-resolving, got %d: %+v", claudeSessions, who.Sessions)
	}
}

// TestMultiplexedAttachFailureFallsBackToPlaceholder: a multiplexed adapter with no meta id
// and no attachable session must not error on every call; it falls back to a placeholder.
func TestMultiplexedAttachFailureFallsBackToPlaceholder(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	opts := Options{
		Dial:      func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters:  adapter.NewRegistry(newFakeMultiplexedAdapter()),
		Getenv:    func(string) string { return "" },
		Cwd:       "/src/fm",
		ParentPID: 9001,
	}
	c := connect(t, ctx, opts)
	for i := 0; i < 2; i++ {
		res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"})
		if err != nil || res.IsError {
			t.Fatalf("call %d: %v %+v", i, err, res)
		}
	}
}

// TestMetaSessionIDTakesPriorityOverAttach: a multiplexed adapter given a meta session id must
// use it directly rather than calling attach (which would fail: nothing is attachable here).
func TestMetaSessionIDTakesPriorityOverAttach(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	opts := Options{
		Dial:      func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters:  adapter.NewRegistry(newFakeMetaAdapter()),
		Getenv:    func(string) string { return "" },
		Cwd:       "/src/meta",
		ParentPID: 9002,
	}
	c := connect(t, ctx, opts)
	res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team", Meta: mcp.Meta{"session_id": "m1"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if !strings.Contains(text(res), "← you") {
		t.Fatalf("expected the meta-resolved session to show itself: %q", text(res))
	}

	res2, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team", Meta: mcp.Meta{"session_id": "m1"}})
	if err != nil || res2.IsError {
		t.Fatalf("call2: %v %+v", err, res2)
	}
	if text(res) != text(res2) {
		t.Fatalf("expected a stable single session across calls with the same meta id: first=%q second=%q", text(res), text(res2))
	}
}

// TestProactiveRedialAfterDaemonRestart: the push loop notices a dead connection on its own
// (no tool call involved) and reconnects to a fresh daemon on the same socket path, replaying
// its registration so a push still reaches the fake Claude inbox.
func TestProactiveRedialAfterDaemonRestart(t *testing.T) {
	ctx := context.Background()

	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")

	ctx1, cancel1 := context.WithCancel(ctx)
	d1 := newTestDaemon(t, func(string) bool { return true })
	l1, err := ipc.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go ipc.Serve(ctx1, l1, d1.Handle, d1.ConnClosed)

	inboxSock, lines := startFakeInbox(t)

	var mu sync.Mutex
	var lastClient *ipc.Client
	dial := func() (*ipc.Client, error) {
		c, err := ipc.Dial(sock)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		lastClient = c
		mu.Unlock()
		return c, nil
	}

	bOpts := Options{
		Dial:     dial,
		Adapters: adapter.NewRegistry(claude.New(), generic.WithEnv("mcp", "AGENTNET_TEST_SESSION")),
		Getenv: func(k string) string {
			switch k {
			case "CLAUDE_CODE_SESSION_ID":
				return "B"
			case "CLAUDE_CODE_MESSAGING_SOCKET":
				return inboxSock
			}
			return ""
		},
		Cwd: "/src/b",
	}
	b := connect(t, ctx, bOpts)
	if _, err := b.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	c1 := lastClient
	mu.Unlock()

	// Simulate a daemon restart: kill daemon #1's listener and B's live connection to it,
	// then start daemon #2 on the same socket path.
	cancel1()
	l1.Close()
	c1.Close()

	ctx2, cancel2 := context.WithCancel(ctx)
	t.Cleanup(cancel2)
	d2 := newTestDaemon(t, func(string) bool { return true })
	l2, err := ipc.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go ipc.Serve(ctx2, l2, d2.Handle, d2.ConnClosed)

	// Poll daemon #2 directly (never through a tool call on b) until B reappears on its own.
	raw, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	deadline := time.After(5 * time.Second)
poll:
	for {
		var who daemon.WhoResult
		if err := raw.Call(ctx, "who", daemon.WhoArgs{}, &who); err == nil {
			for _, s := range who.Sessions {
				if s.Agent == "claude-code" {
					break poll
				}
			}
		}
		select {
		case <-deadline:
			t.Fatal("B never reappeared on the new daemon without a tool call")
		case <-time.After(100 * time.Millisecond):
		}
	}

	env2 := &testEnv{sock: sock}
	a := connect(t, ctx, env2.options("A", "/src/a"))
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "alex/b-claude", "message": "after restart"}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}
	waitForUserFrame(t, lines, 5*time.Second)
}

// TestDefaultLoggerIsStderr: Options.Logger defaults to a non-nil stderr logger, so
// check_inbox's "delivered" error logging never panics on a nil logger.
func TestDefaultLoggerIsStderr(t *testing.T) {
	sm := newSessionManager(context.Background(), Options{})
	if sm.o.Logger == nil {
		t.Fatal("expected a default logger")
	}
}

// TestStaleClaudeSessionNeverReceivesAPushAfterRegistryChange: register c1, switch the
// registry to c2 while c1's hooks keep it live, then send to c1's own name. Nothing must be
// posted to the fake inbox socket (which now belongs to c2's conversation), and c1's mail
// must stay pending rather than being silently lost.
func TestStaleClaudeSessionNeverReceivesAPushAfterRegistryChange(t *testing.T) {
	ctx := context.Background()
	env := startDaemon(t)
	inboxSock, lines := startFakeInbox(t)

	dir := t.TempDir()
	writeRegistry := func(sessionID string) {
		b, _ := json.Marshal(map[string]any{"sessionId": sessionID})
		if err := os.WriteFile(filepath.Join(dir, "0.json"), b, 0o644); err != nil { // ParentPID left 0 below
			t.Fatal(err)
		}
	}
	writeRegistry("c1")

	cOpts := Options{
		Dial:     func() (*ipc.Client, error) { return ipc.Dial(env.sock) },
		Adapters: adapter.NewRegistry(claude.New()),
		Getenv: func(k string) string {
			switch k {
			case "CLAUDE_CODE_SESSION_ID":
				return "env-placeholder" // only picks the claude-code adapter; the registry file wins the id
			case "CLAUDE_CODE_MESSAGING_SOCKET":
				return inboxSock
			}
			return ""
		},
		Cwd:       "/src/stale",
		ClaudeDir: dir,
	}
	c := connect(t, ctx, cOpts)
	if _, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	raw, err := ipc.Dial(env.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var who daemon.WhoResult
	if err := raw.Call(ctx, "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatal(err)
	}
	var c1Addr string
	for _, s := range who.Sessions {
		if s.Agent == "claude-code" {
			c1Addr = s.Address
		}
	}
	if c1Addr == "" {
		t.Fatal("c1 never registered")
	}

	// c1's hooks just ran: it stays live (within hookGrace) even after the registry moves on.
	if err := raw.Call(ctx, "hook", daemon.HookArgs{Agent: "claude-code", Event: adapter.EventStart, SessionID: "c1", Cwd: "/src/stale"}, &daemon.HookResult{}); err != nil {
		t.Fatal(err)
	}

	writeRegistry("c2")
	if _, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}

	a := connect(t, ctx, env.options("A", "/src/a"))
	if _, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "list_team"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": c1Addr, "message": "for c1 only"}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}

	select {
	case line := <-lines:
		t.Fatalf("the stale session's mail was posted to the (now c2's) inbox socket: %q", line)
	case <-time.After(500 * time.Millisecond):
	}

	// c1's mail must still be there, unread, waiting for it to be picked up (e.g. by check_inbox).
	var unread daemon.UnreadResult
	c1Key := "claude-code:c1"
	if err := raw.Call(ctx, "unread", daemon.InboxArgs{Key: c1Key}, &unread); err != nil {
		t.Fatal(err)
	}
	if unread.Count == 0 {
		t.Fatal("c1's mail is gone instead of staying pending")
	}
}
