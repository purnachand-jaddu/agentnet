package hook

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/daemon"
)

// init stubs agentPID for the whole test binary: tests exercise Run's own logic, not ps(1).
func init() {
	agentPID = func(start int) int { return start }
}

// TestPollEventUsesFlagsNotStdin checks the plugin-driven events: the flag session id overrides the
// empty execFile stdin, and the state rides through to the daemon.
func TestPollAndStatusEventsUseFlagsAndMapEvents(t *testing.T) {
	var got daemon.HookArgs
	call := func(_ context.Context, op string, args, result any) error {
		if op == "hook" {
			got = args.(daemon.HookArgs)
			*(result.(*daemon.HookResult)) = daemon.HookResult{}
		}
		return nil
	}
	var out bytes.Buffer

	Run(context.Background(), "opencode", "poll", bytes.NewReader(nil), &out, call, func(string) string { return "" },
		Flags{SessionID: "ses_abc"})
	if got.SessionID != "ses_abc" || got.Event != adapter.EventPoll {
		t.Fatalf("poll args = %+v", got)
	}

	Run(context.Background(), "opencode", "status", strings.NewReader(`{"session_id":"from_stdin"}`), &out, call, func(string) string { return "" },
		Flags{SessionID: "ses_abc", State: "busy"})
	if got.SessionID != "ses_abc" || got.Event != adapter.EventStatus || got.State != "busy" {
		t.Fatalf("status args = %+v", got)
	}
}
