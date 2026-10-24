// Package ipc is the newline-delimited JSON protocol between the agentnet
// daemon and its clients (MCP server, hooks, CLI) over a Unix socket.
package ipc

import (
	"context"
	"encoding/json"
)

// maxLine is the largest NDJSON line either side will read.
const maxLine = 1 << 20

// Request is a client call to the daemon.
type Request struct {
	ID   uint64          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Reply answers a Request with the matching ID.
type Reply struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Event is pushed by the daemon to a connection, e.g. mail for a session it serves.
type Event struct {
	Event string   `json:"event"` // "deliver"
	Key   string   `json:"key"`
	IDs   []string `json:"ids"`
	Text  string   `json:"text"`
}

// Conn is one client connection as the daemon sees it.
type Conn interface {
	ID() uint64
	Push(ev Event) error
}

// Handler answers one Request's op, returning a result to marshal or an error.
type Handler func(ctx context.Context, conn Conn, op string, args json.RawMessage) (any, error)
