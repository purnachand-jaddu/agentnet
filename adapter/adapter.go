// Package adapter abstracts what is specific to each coding agent: identity, push and hook output.
package adapter

import (
	"context"
	"errors"
)

// Tier ranks how reachable a session is.
type Tier int

const (
	Pull Tier = iota
	Turn
	Push
)

// Session is one live agent conversation.
type Session struct {
	Key        string `json:"key"`   // "<agent>:<id>"
	Agent      string `json:"agent"` // adapter name
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	PID        int    `json:"pid,omitempty"`         // the agent's own process
	Transcript string `json:"transcript,omitempty"`  // from hooks
	Inbox      string `json:"inbox,omitempty"`       // agent push endpoint, e.g. Claude's socket
	InboxToken string `json:"inbox_token,omitempty"` // that session's own token, never the daemon's
}

// Channel is how a message reaches a session right now.
type Channel int

const (
	ChannelPull    Channel = iota // the agent calls check_inbox
	ChannelHook                   // the agent's next hook prints it
	ChannelMCP                    // the session's own MCP process pushes it
	ChannelCommand                // the daemon pushes it with Adapter.Push
)

// State is what the daemon knows about reaching a session.
type State struct {
	MCPPush    bool // its MCP process is connected and can push
	HookSeen   bool // its hooks have run
	PushFailed bool // the last daemon-side push failed
}

// HookEvent names a point in an agent's lifecycle that runs a hook.
type HookEvent string

const (
	EventStart  HookEvent = "start"
	EventPrompt HookEvent = "prompt"
	EventTool   HookEvent = "tool"
	EventStop   HookEvent = "stop"
	EventEnd    HookEvent = "end"
)

// Adapter is everything agent-specific: identity, push and hook output.
type Adapter interface {
	Name() string
	Short() string // used in session names: "claude", "codex"
	EnvSessionID(getenv func(string) string) string
	MetaSessionID(meta map[string]any) string
	Multiplexed() bool // one MCP process serves many sessions
	Channel(s Session, st State) Channel
	Push(ctx context.Context, s Session, text string) error
	HookOutput(ev HookEvent, text string) (out string, ok bool)
	Tier(s Session, st State) Tier
}

// TurnWatcher is implemented by adapters that can tell when a turn ended without a Stop hook.
type TurnWatcher interface {
	TurnRunning(s Session) (running, known bool)
}

// ClientNamer is implemented by adapters whose MCP client reports a name other than Name() (e.g. "codex-mcp-client").
type ClientNamer interface {
	ClientNames() []string
}

// ErrCannotPush is returned by Push when the adapter has no way to push to the session.
var ErrCannotPush = errors.New("this session can't be pushed to")

// Registry looks up adapters by name, client string or environment.
type Registry struct{ list []Adapter }

// NewRegistry builds a Registry over as.
func NewRegistry(as ...Adapter) *Registry { return &Registry{list: as} }

// Get returns the adapter named name.
func (r *Registry) Get(name string) (Adapter, bool) {
	for _, a := range r.list {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}

// FromClient matches an MCP client name exactly: first against any adapter's ClientNames, then against Name().
func (r *Registry) FromClient(clientName string) (Adapter, bool) {
	for _, a := range r.list {
		if cn, ok := a.(ClientNamer); ok {
			for _, n := range cn.ClientNames() {
				if n == clientName {
					return a, true
				}
			}
		}
	}
	for _, a := range r.list {
		if a.Name() == clientName {
			return a, true
		}
	}
	return nil, false
}

// FromEnv returns the first adapter whose EnvSessionID reports a non-empty id.
func (r *Registry) FromEnv(getenv func(string) string) (Adapter, bool) {
	for _, a := range r.list {
		if a.EnvSessionID(getenv) != "" {
			return a, true
		}
	}
	return nil, false
}
