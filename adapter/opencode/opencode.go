// Package opencode is the OpenCode adapter: session ids from MCP call meta, hook-seen turn tier.
// OpenCode has no command hooks; its plugin (written by `agentnet setup`) reports session state
// and polls for mail, so delivery waits for its next idle poll.
package opencode

import (
	"context"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

// Adapter is a pull-turn adapter: mail printed by its poll hook, never pushed.
type Adapter struct{}

// New returns the OpenCode adapter.
func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string  { return "opencode" }
func (*Adapter) Short() string { return "opencode" }

// EnvSessionID reads OPENCODE_SESSION_ID, when one was exported into the MCP server's environment.
func (*Adapter) EnvSessionID(getenv func(string) string) string {
	return getenv("OPENCODE_SESSION_ID")
}

// MetaSessionID reads ai.opencode/sessionID, which OpenCode 2.x sends on every tool call.
func (*Adapter) MetaSessionID(meta map[string]any) string {
	if id, ok := meta["ai.opencode/sessionID"].(string); ok {
		return id
	}
	return ""
}

// Multiplexed reports whether one MCP process serves many sessions.
func (*Adapter) Multiplexed() bool { return true }

// Channel returns how a message reaches the session given its state: hooks print mail only once
// the plugin has polled once (HookSeen), so state starts pull-only.
func (_ *Adapter) Channel(_ adapter.Session, st adapter.State) adapter.Channel {
	if st.HookSeen {
		return adapter.ChannelHook
	}
	return adapter.ChannelPull
}

// Push has no mechanism: OpenCode offers no daemon-facing wake or queue command.
func (*Adapter) Push(context.Context, adapter.Session, string) error { return adapter.ErrCannotPush }

// HookOutput returns mail as plain text for poll and stop; the plugin injects it into the session.
func (*Adapter) HookOutput(ev adapter.HookEvent, text string) (string, bool) {
	switch ev {
	case adapter.EventPoll, adapter.EventStop:
		return text, true
	default:
		return "", false
	}
}

// Tier derives reachability from the channel.
func (a *Adapter) Tier(s adapter.Session, st adapter.State) adapter.Tier {
	if a.Channel(s, st) == adapter.ChannelHook {
		return adapter.Turn
	}
	return adapter.Pull
}

// Compile-time check that Adapter implements adapter.Adapter.
var _ adapter.Adapter = (*Adapter)(nil)
