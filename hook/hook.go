// Package hook is the entry an agent's hook system runs: agentnet hook <agent> <event>.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/internal/proc"
)

// agentPID finds the agent's real pid past any shell that execs the hook; tests stub it to stay hermetic.
var agentPID = func(start int) int { return proc.AgentPID(start, proc.PS) }

// Flags are the command-line flags that override stdin-JSON fields (an execFile caller passes no stdin).
type Flags struct {
	SessionID string // --session
	State     string // --state: opencode's status event, "busy" or "idle"
}

// eventByName maps a hook system's event name to the adapter.HookEvent the daemon expects.
var eventByName = map[string]adapter.HookEvent{
	"session-start":      adapter.EventStart,
	"user-prompt-submit": adapter.EventPrompt,
	"post-tool-use":      adapter.EventTool,
	"stop":               adapter.EventStop,
	"session-end":        adapter.EventEnd,
	"poll":               adapter.EventPoll,
	"status":             adapter.EventStatus,
}

// stdinJSON is the subset of a hook's JSON payload Run reads.
type stdinJSON struct {
	SessionID      string `json:"session_id"`
	SessionIDAlt   string `json:"sessionId"`
	ConversationID string `json:"conversation_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
}

var bom = []byte{0xEF, 0xBB, 0xBF}

// readStdin reads up to 1 MiB from r, giving up (and returning whatever has arrived, possibly nothing) after timeout.
func readStdin(r io.Reader, timeout time.Duration) []byte {
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
		ch <- b
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(timeout):
		return nil
	}
}

// Run reads the hook's stdin JSON (or Flags), tells the daemon about event, and prints any mail.
// It never returns an error: a broken daemon or malformed input must not break the agent that ran the hook.
func Run(ctx context.Context, agent, event string, stdin io.Reader, stdout io.Writer, call func(ctx context.Context, op string, args, result any) error, getenv func(string) string, flags Flags) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	ev, ok := eventByName[event]
	if !ok {
		return nil
	}

	var in stdinJSON
	sessionID := flags.SessionID
	if sessionID == "" {
		// A --session caller (opencode's plugin) passes an open-but-empty stdin pipe; reading it
		// would burn the 3-second deadline on every call, so flags carry the whole stdin contract.
		json.Unmarshal(bytes.TrimPrefix(readStdin(stdin, 3*time.Second), bom), &in) // best effort
		sessionID = in.SessionID
	}
	if sessionID == "" {
		sessionID = in.SessionIDAlt
	}
	if sessionID == "" {
		sessionID = in.ConversationID
	}
	if sessionID == "" {
		return nil
	}

	cwd := in.Cwd
	if cwd == "" {
		cwd = getenv("CLAUDE_PROJECT_DIR")
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	args := daemon.HookArgs{
		Agent: agent, Event: ev, SessionID: sessionID, Cwd: cwd, State: flags.State, PID: agentPID(os.Getppid()), Transcript: in.TranscriptPath,
	}
	if agent == "claude-code" {
		args.Inbox = getenv("CLAUDE_CODE_MESSAGING_SOCKET")
		args.InboxToken = getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	}

	var res daemon.HookResult
	if err := call(ctx, "hook", args, &res); err != nil {
		return nil
	}
	if res.Out != "" {
		if _, err := io.WriteString(stdout, res.Out+"\n"); err != nil {
			return nil // couldn't show the mail, so don't confirm it as delivered
		}
	}
	if len(res.IDs) > 0 {
		call(ctx, "delivered", daemon.DeliveredArgs{Key: res.Key, IDs: res.IDs}, nil)
	}
	return nil
}
