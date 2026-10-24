// Package codex is the Codex adapter: codex queue push, rollout turn detection, hook output.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

// Runner is a command runner for testing or execution.
type Runner func(ctx context.Context, name string, args ...string) error

// Adapter pushes to Codex via codex queue command.
type Adapter struct {
	run      Runner
	lookPath func(string) (string, error)
}

// New creates a Codex adapter with the given runner.
func New(run Runner) *Adapter {
	if run == nil {
		run = ExecRunner
	}
	return &Adapter{
		run:      run,
		lookPath: exec.LookPath,
	}
}

// Name reports the adapter identity.
func (*Adapter) Name() string { return "codex" }

// Short is the abbreviated name.
func (*Adapter) Short() string { return "codex" }

// EnvSessionID reads CODEX_THREAD_ID.
func (*Adapter) EnvSessionID(getenv func(string) string) string {
	return getenv("CODEX_THREAD_ID")
}

// MetaSessionID checks sessionId then threadId.
func (*Adapter) MetaSessionID(meta map[string]any) string {
	if id, ok := meta["sessionId"].(string); ok && id != "" {
		return id
	}
	if id, ok := meta["threadId"].(string); ok && id != "" {
		return id
	}
	return ""
}

// Multiplexed reports whether one MCP process serves many sessions.
func (*Adapter) Multiplexed() bool { return true }

// Channel returns how a message reaches the session given its state.
func (a *Adapter) Channel(s adapter.Session, st adapter.State) adapter.Channel {
	running, _ := TurnRunning(s.Transcript)
	if running {
		return adapter.ChannelHook
	}
	if !st.PushFailed {
		if _, err := a.lookPath("codex"); err == nil {
			return adapter.ChannelCommand
		}
	}
	if st.HookSeen {
		return adapter.ChannelHook
	}
	return adapter.ChannelPull
}

// Push sends text to the session via codex queue command.
func (a *Adapter) Push(ctx context.Context, s adapter.Session, text string) error {
	return a.run(ctx, "codex", "queue", "--thread", s.ID, "--message="+text)
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
	if ch == adapter.ChannelCommand {
		return adapter.Push
	}
	if ch == adapter.ChannelHook {
		return adapter.Turn
	}
	return adapter.Pull
}

// ClientNames reports the MCP client names this adapter serves.
func (*Adapter) ClientNames() []string { return []string{"codex-mcp-client"} }

// TurnRunning reports whether a turn is running by checking the rollout transcript.
func (*Adapter) TurnRunning(s adapter.Session) (running, known bool) {
	return TurnRunning(s.Transcript)
}

// ExecRunner runs a command with exec.CommandContext, capturing output.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Compile-time check that Adapter implements adapter.Adapter, adapter.TurnWatcher and adapter.ClientNamer.
var _ adapter.Adapter = (*Adapter)(nil)
var _ adapter.TurnWatcher = (*Adapter)(nil)
var _ adapter.ClientNamer = (*Adapter)(nil)
