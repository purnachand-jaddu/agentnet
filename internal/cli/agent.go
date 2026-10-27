package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	agent, event, flags, ok := hookArgs(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: agentnet hook <agent> <event> [--session id] [--state busy|idle]")
		return 0
	}

	call := func(ctx context.Context, op string, args, result any) error {
		return fmt.Errorf("agentnet: daemon unavailable")
	}
	if client, err := autostartDial(); err == nil {
		defer client.Close()
		call = client.Call
	}
	hook.Run(context.Background(), agent, event, stdin, stdout, call, os.Getenv, flags)
	return 0
}

// hookArgs parses `agentnet hook` arguments: agent and event are positional, and --session/--state may
// sit on either side of them — opencode's plugin calls with the positionals first, and Go's flag
// package stops parsing at the first positional, so the two known flags are pulled out manually.
func hookArgs(args []string) (agent, event string, flags hook.Flags, ok bool) {
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--session" && i+1 < len(args):
			flags.SessionID = args[i+1]
			i++
		case arg == "--state" && i+1 < len(args):
			flags.State = args[i+1]
			i++
		case strings.HasPrefix(arg, "--session="):
			flags.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--state="):
			flags.State = strings.TrimPrefix(arg, "--state=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return "", "", hook.Flags{}, false // an unknown value-less flag: usage, as before
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 2 {
		return "", "", hook.Flags{}, false
	}
	return positional[0], positional[1], flags, true
}
