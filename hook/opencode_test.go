package hook

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/daemon"
)

// init stubs agentPID for the whole test binary: tests exercise Run's own logic, not ps(1).
func init() {
	agentPID = func(start int) int { return start }
}

// TestFlagSessionSkipsStdinRead pins the plugin's runtime condition: execFile leaves stdin as an
// open, empty pipe, and Run with --session must answer without waiting the 3-second read deadline
// on stdin it will not use.
func TestFlagSessionSkipsStdinRead(t *testing.T) {
	var got daemon.HookArgs
	call := func(_ context.Context, op string, args, result any) error {
		if op == "hook" {
			got = args.(daemon.HookArgs)
		}
		return nil
	}
	pr, pw := io.Pipe()
	defer pw.Close()

	start := time.Now()
	err := Run(context.Background(), "opencode", "poll", pr, io.Discard, call, func(string) string { return "" },
		Flags{SessionID: "ses_x"})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Fatalf("waited %v for unused stdin", d)
	}
	if got.SessionID != "ses_x" || got.Event != adapter.EventPoll {
		t.Fatalf("args = %+v", got)
	}
}

// TestPollAndStatusEventsUseFlagsAndMapEvents checks the plugin-driven events: the flag session id overrides the
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
