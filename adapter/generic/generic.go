// Package generic is a pull-only adapter for any MCP client that has no agent-specific support.
package generic

import (
	"context"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

// Adapter is a pull-only adapter: the agent must call check_inbox, nothing is pushed.
type Adapter struct {
	name   string
	envVar string
}

// New returns the default generic adapter, named "mcp".
func New() *Adapter { return &Adapter{name: "mcp"} }

// WithEnv returns a generic adapter named name whose EnvSessionID reads envVar.
func WithEnv(name, envVar string) *Adapter { return &Adapter{name: name, envVar: envVar} }

func (a *Adapter) Name() string  { return a.name }
func (a *Adapter) Short() string { return a.name }

func (a *Adapter) EnvSessionID(getenv func(string) string) string {
	if a.envVar == "" {
		return ""
	}
	return getenv(a.envVar)
}

func (a *Adapter) MetaSessionID(map[string]any) string { return "" }

func (a *Adapter) Multiplexed() bool { return false }

func (a *Adapter) Channel(adapter.Session, adapter.State) adapter.Channel { return adapter.ChannelPull }

func (a *Adapter) Push(context.Context, adapter.Session, string) error { return adapter.ErrCannotPush }

func (a *Adapter) HookOutput(adapter.HookEvent, string) (string, bool) { return "", false }

func (a *Adapter) Tier(adapter.Session, adapter.State) adapter.Tier { return adapter.Pull }
