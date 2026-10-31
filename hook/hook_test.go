package hook

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/daemon"
)

// init stubs agentPID for the whole test binary: tests exercise Run's own logic, not ps(1).
func init() {
	agentPID = func(start int) int { return start }
}

func TestPrintsMailThenConfirms(t *testing.T) {
	var ops []string
	var gotArgs daemon.HookArgs
	call := func(_ context.Context, op string, args, result any) error {
		ops = append(ops, op)
		if op == "hook" {
			gotArgs = args.(daemon.HookArgs)
			*(result.(*daemon.HookResult)) = daemon.HookResult{Out: "X", Key: "codex:s1", IDs: []string{"m1"}}
		}
		return nil
	}
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"s1","cwd":"/src/api","transcript_path":"/t"}`)
	if err := Run(context.Background(), "codex", "post-tool-use", in, &out, call, func(string) string { return "" }, Flags{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "X\n" || strings.Join(ops, ",") != "hook,delivered" || gotArgs.Event != "tool" || gotArgs.Transcript != "/t" {
		t.Fatalf("out %q ops %v args %+v", out.String(), ops, gotArgs)
	}
}

func TestBrokenDaemonNeverBreaksTheAgent(t *testing.T) {
	var out bytes.Buffer
	call := func(context.Context, string, any, any) error { return errors.New("down") }
	if err := Run(context.Background(), "codex", "stop", strings.NewReader(`{"session_id":"s1"}`), &out, call, func(string) string { return "" }, Flags{}); err != nil || out.Len() != 0 {
		t.Fatalf("err %v out %q", err, out.String())
	}
}

// errWriter always fails, simulating the agent having closed the hook's stdout.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// TestWriteErrorSkipsDelivered checks that a failed print never confirms mail as delivered: the
// agent never saw it, so marking it read would lose it.
func TestWriteErrorSkipsDelivered(t *testing.T) {
	var ops []string
	call := func(_ context.Context, op string, args, result any) error {
		ops = append(ops, op)
		if op == "hook" {
			*(result.(*daemon.HookResult)) = daemon.HookResult{Out: "X", Key: "codex:s1", IDs: []string{"m1"}}
		}
		return nil
	}
	if err := Run(context.Background(), "codex", "stop", strings.NewReader(`{"session_id":"s1"}`), errWriter{}, call, func(string) string { return "" }, Flags{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ops, ",") != "hook" {
		t.Fatalf("expected no delivered call after a write error, got ops %v", ops)
	}
}

// TestInvalidStdinNoSessionIDMakesNoCalls checks that unparseable stdin with no session id is silent, not a call.
func TestInvalidStdinNoSessionIDMakesNoCalls(t *testing.T) {
	calls := 0
	call := func(context.Context, string, any, any) error { calls++; return nil }
	var out bytes.Buffer
	if err := Run(context.Background(), "codex", "stop", strings.NewReader("not json"), &out, call, func(string) string { return "" }, Flags{}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || out.Len() != 0 {
		t.Fatalf("calls %d out %q", calls, out.String())
	}
}

// TestUnknownEventMakesNoCalls checks that an event with no mapping is a silent no-op.
func TestUnknownEventMakesNoCalls(t *testing.T) {
	calls := 0
	call := func(context.Context, string, any, any) error { calls++; return nil }
	var out bytes.Buffer
	if err := Run(context.Background(), "codex", "weird-event", strings.NewReader(`{"session_id":"s1"}`), &out, call, func(string) string { return "" }, Flags{}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("calls %d", calls)
	}
}

// TestClaudeEnvInboxOnlyForClaudeCode checks that inbox env vars are read for claude-code only.
func TestClaudeEnvInboxOnlyForClaudeCode(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_MESSAGING_SOCKET": "/tmp/sock", "CLAUDE_CODE_MESSAGING_TOKEN": "tok"}
	getenv := func(k string) string { return env[k] }
	var gotArgs daemon.HookArgs
	call := func(_ context.Context, op string, args, result any) error {
		if op == "hook" {
			gotArgs = args.(daemon.HookArgs)
			*(result.(*daemon.HookResult)) = daemon.HookResult{}
		}
		return nil
	}
	var out bytes.Buffer

	Run(context.Background(), "claude-code", "stop", strings.NewReader(`{"session_id":"s1"}`), &out, call, getenv, Flags{})
	if gotArgs.Inbox != "/tmp/sock" || gotArgs.InboxToken != "tok" {
		t.Fatalf("claude-code args = %+v", gotArgs)
	}

	gotArgs = daemon.HookArgs{}
	Run(context.Background(), "codex", "stop", strings.NewReader(`{"session_id":"s1"}`), &out, call, getenv, Flags{})
	if gotArgs.Inbox != "" || gotArgs.InboxToken != "" {
		t.Fatalf("codex args should have empty inbox fields, got %+v", gotArgs)
	}
}
