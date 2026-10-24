package mcpserver

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/claude"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

func text(r *mcp.CallToolResult) string { return r.Content[0].(*mcp.TextContent).Text }

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

	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	inboxSock := filepath.Join(dir, "inbox.sock")

	ln, err := net.Listen("unix", inboxSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	lines := make(chan string, 8)
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

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, `"type":"user"`) {
				goto delivered
			}
		case <-deadline:
			t.Fatal("inbox listener never received a user frame")
		}
	}
delivered:

	deadline = time.After(5 * time.Second)
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
