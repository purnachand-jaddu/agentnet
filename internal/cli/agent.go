package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/purnachand-jaddu/agentnet/hook"
	"github.com/purnachand-jaddu/agentnet/internal/proc"
	"github.com/purnachand-jaddu/agentnet/mcpserver"
)

// cmdMCP runs `agentnet mcp`, the stdio MCP server coding agents load.
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	o := mcpserver.Options{
		Dial:      autostartDial,
		Adapters:  adapters(),
		Getenv:    os.Getenv,
		Cwd:       cwd,
		ParentPID: proc.AgentPID(os.Getppid(), proc.PS),
		ClaudeDir: filepath.Join(home, ".claude", "sessions"),
	}
	if err := mcpserver.Run(context.Background(), &mcp.StdioTransport{}, o); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// cmdHook runs `agentnet hook <agent> <event>`; it always exits 0 so a broken hook never breaks the agent.
func cmdHook(args []string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 0
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: agentnet hook <agent> <event>")
		return 0
	}

	call := func(ctx context.Context, op string, args, result any) error {
		return fmt.Errorf("agentnet: daemon unavailable")
	}
	if client, err := autostartDial(); err == nil {
		defer client.Close()
		call = client.Call
	}
	hook.Run(context.Background(), rest[0], rest[1], stdin, stdout, call, os.Getenv)
	return 0
}
