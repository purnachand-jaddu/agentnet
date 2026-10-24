package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

// Adapter pushes to Claude Code via socket and inbox token.
type Adapter struct{}

// New creates a Claude Code adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name reports the adapter identity.
func (*Adapter) Name() string { return "claude-code" }

// Short is the abbreviated name.
func (*Adapter) Short() string { return "claude" }

// EnvSessionID reads CLAUDE_CODE_SESSION_ID.
func (*Adapter) EnvSessionID(getenv func(string) string) string {
	return getenv("CLAUDE_CODE_SESSION_ID")
}

// MetaSessionID returns empty string.
func (*Adapter) MetaSessionID(map[string]any) string { return "" }

// Multiplexed reports whether one MCP process serves many sessions.
func (*Adapter) Multiplexed() bool { return false }

// Channel returns how a message reaches the session given its state.
func (*Adapter) Channel(s adapter.Session, st adapter.State) adapter.Channel {
	if st.MCPPush {
		return adapter.ChannelMCP
	}
	if s.Inbox != "" && !st.PushFailed {
		return adapter.ChannelCommand
	}
	if st.HookSeen {
		return adapter.ChannelHook
	}
	return adapter.ChannelPull
}

// Push sends text to the session's inbox socket with its own token.
func (*Adapter) Push(ctx context.Context, s adapter.Session, text string) error {
	return PostInbox(s.Inbox, s.InboxToken, text)
}

// HookOutput formats text for a hook event as JSON.
func (*Adapter) HookOutput(ev adapter.HookEvent, text string) (string, bool) {
	switch ev {
	case adapter.EventStart:
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": text,
			},
		}
		b, _ := json.Marshal(out)
		return string(b), true
	case adapter.EventPrompt:
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": text,
			},
		}
		b, _ := json.Marshal(out)
		return string(b), true
	case adapter.EventTool:
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": text,
			},
		}
		b, _ := json.Marshal(out)
		return string(b), true
	case adapter.EventStop:
		out := struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}{
			Decision: "block",
			Reason:   text,
		}
		b, _ := json.Marshal(out)
		return string(b), true
	case adapter.EventEnd:
		return "", false
	}
	return "", false
}

// Tier derives reachability from the channel.
func (a *Adapter) Tier(s adapter.Session, st adapter.State) adapter.Tier {
	ch := a.Channel(s, st)
	if ch == adapter.ChannelMCP || ch == adapter.ChannelCommand {
		return adapter.Push
	}
	if ch == adapter.ChannelHook {
		return adapter.Turn
	}
	return adapter.Pull
}

// ClientNames reports the MCP client names this adapter serves.
func (*Adapter) ClientNames() []string { return []string{"claude-code"} }

// PostInbox dials the inbox socket, writes auth and user frames, and closes.
func PostInbox(socket string, token string, text string) error {
	if socket == "" {
		return fmt.Errorf("empty socket path")
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	if token != "" {
		authFrame := map[string]any{
			"type":  "auth",
			"token": token,
		}
		b, _ := json.Marshal(authFrame)
		if _, err := conn.Write(append(b, '\n')); err != nil {
			return err
		}
	}

	userFrame := map[string]any{
		"type": "user",
		"message": map[string]string{
			"role":    "user",
			"content": text,
		},
	}
	b, _ := json.Marshal(userFrame)
	_, err = conn.Write(append(b, '\n'))
	return err
}

// Compile-time check that Adapter implements adapter.Adapter and adapter.ClientNamer.
var _ adapter.Adapter = (*Adapter)(nil)
var _ adapter.ClientNamer = (*Adapter)(nil)
