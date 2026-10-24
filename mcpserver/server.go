// Package mcpserver is the stdio MCP server coding agents load to message each other,
// a thin client of the agentnet daemon over its Unix socket.
package mcpserver

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// Options configures Run.
type Options struct {
	Dial      func() (*ipc.Client, error)
	Adapters  *adapter.Registry
	Getenv    func(string) string
	Cwd       string
	ParentPID int         // os.Getppid(): the agent process
	ClaudeDir string      // ~/.claude/sessions, for the undocumented fallback
	Logger    *log.Logger // defaults to stderr; never stdout, which is the MCP stream
}

// defaultLogger writes to stderr, since stdout carries the MCP protocol stream.
func defaultLogger() *log.Logger { return log.New(os.Stderr, "agentnet-mcp: ", 0) }

// ListArgs is the list_team tool's arguments.
type ListArgs struct {
	Filter string `json:"filter,omitempty" jsonschema:"only sessions matching this text"`
}

// SendArgs is the send_message tool's arguments.
type SendArgs struct {
	To      string `json:"to" jsonschema:"person/session, as list_team shows it"`
	Message string `json:"message" jsonschema:"the message, point first"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"id of the message you are answering"`
}

// InboxArgs is the check_inbox tool's arguments.
type InboxArgs struct {
	WaitSecs int `json:"wait_secs,omitempty" jsonschema:"wait up to this many seconds for a message (max 600)"`
}

// Run starts the agentnet MCP server on t and blocks until the client disconnects or ctx is done.
func Run(ctx context.Context, t mcp.Transport, o Options) error {
	sm := newSessionManager(ctx, o)
	s := mcp.NewServer(&mcp.Implementation{Name: "agentnet", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{Name: "list_team", Description: "List the other agentnet sessions on this machine"}, sm.listTeam)
	mcp.AddTool(s, &mcp.Tool{Name: "send_message", Description: "Send a message to another agentnet session"}, sm.sendMessage)
	mcp.AddTool(s, &mcp.Tool{Name: "check_inbox", Description: "Check your inbox for messages"}, sm.checkInbox)

	return s.Run(ctx, t)
}

// listTeam handles the list_team tool.
func (sm *sessionManager) listTeam(ctx context.Context, req *mcp.CallToolRequest, in ListArgs) (*mcp.CallToolResult, any, error) {
	key, err := sm.resolveSession(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var res daemon.WhoResult
	if err := sm.call(ctx, "who", daemon.WhoArgs{Key: key}, &res); err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	for _, e := range res.Sessions {
		line := fmt.Sprintf("%s  %s  %s  %s@%s", e.Address, e.Agent, e.Status, e.Repo, e.Branch)
		if in.Filter != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(in.Filter)) {
			continue
		}
		if e.You {
			line += "  ← you"
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	sm.appendUnread(ctx, &b, key)
	return textResult(b.String()), nil, nil
}

// sendMessage handles the send_message tool.
func (sm *sessionManager) sendMessage(ctx context.Context, req *mcp.CallToolRequest, in SendArgs) (*mcp.CallToolResult, any, error) {
	key, err := sm.resolveSession(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var res daemon.SendResult
	args := daemon.SendArgs{From: key, To: in.To, Body: in.Message, ReplyTo: in.ReplyTo, Wake: true}
	if err := sm.call(ctx, "send", args, &res); err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	for _, r := range res.Receipts {
		fmt.Fprintf(&b, "%s: %s — %s\n", r.To, r.State, r.Note)
	}
	sm.appendUnread(ctx, &b, key)
	return textResult(b.String()), nil, nil
}

// checkInbox handles the check_inbox tool.
func (sm *sessionManager) checkInbox(ctx context.Context, req *mcp.CallToolRequest, in InboxArgs) (*mcp.CallToolResult, any, error) {
	key, err := sm.resolveSession(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var res daemon.InboxResult
	args := daemon.InboxArgs{Key: key, WaitSecs: in.WaitSecs, Peek: true}
	if err := sm.call(ctx, "inbox", args, &res); err != nil {
		return nil, nil, err
	}

	text := res.Text
	if text == "" {
		text = "No messages."
	}
	if len(res.IDs) > 0 {
		if err := sm.call(ctx, "delivered", daemon.DeliveredArgs{Key: key, IDs: res.IDs}, nil); err != nil {
			sm.o.Logger.Printf("delivered %v: %v", res.IDs, err)
		}
	}
	return textResult(text), nil, nil
}

// appendUnread appends the unread notice to b for key, if there's anything unread.
func (sm *sessionManager) appendUnread(ctx context.Context, b *strings.Builder, key string) {
	var res daemon.UnreadResult
	if err := sm.call(ctx, "unread", daemon.InboxArgs{Key: key}, &res); err != nil || res.Count == 0 {
		return
	}
	seen := map[string]bool{}
	var froms []string
	for _, f := range res.From {
		if !seen[f] {
			seen[f] = true
			froms = append(froms, f)
		}
	}
	fmt.Fprintf(b, "[agentnet] %d unread message(s) from %s. Call check_inbox to read.\n", res.Count, strings.Join(froms, ", "))
}

// textResult wraps s as a CallToolResult's sole text content.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
