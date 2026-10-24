package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// RegisterArgs is the "register" op's arguments.
type RegisterArgs struct {
	Session adapter.Session `json:"session"`
	MCPPush bool            `json:"mcp_push"`
	Name    string          `json:"name,omitempty"`
	MCPPID  int             `json:"mcp_pid,omitempty"` // the MCP process's parent (the agent)
}

// Registered answers a "register" op.
type Registered struct {
	Key     string `json:"key"`
	Address string `json:"address"`
	Name    string `json:"name"`
}

// UnregisterArgs is the "unregister" op's arguments.
type UnregisterArgs struct {
	Key string `json:"key"`
}

// Handle dispatches one op to the matching handler.
func (d *Daemon) Handle(ctx context.Context, c ipc.Conn, op string, args json.RawMessage) (any, error) {
	switch op {
	case "register":
		var a RegisterArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		return d.register(c, a)
	case "unregister":
		var a UnregisterArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		d.unregister(a.Key)
		return nil, nil
	case "shutdown":
		d.doShutdown()
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown op %q", op)
	}
}

// doShutdown closes Shutdown() the first time it's called; later calls are harmless.
func (d *Daemon) doShutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.shutdown:
	default:
		close(d.shutdown)
	}
}
