package cli

import (
	"context"
	"io"
	"testing"

	"github.com/purnachand-jaddu/agentnet/hook"
)

// TestHookArgsParsesPluginInvocation pins the plugin's exact argv shapes: opencode's execFile puts
// the positional agent/event first and the flags last -- Go's flag package stops at the first
// positional, so cmdHook must pull the known flags out regardless of order.
func TestHookArgsParsesPluginInvocation(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		agent string
		event string
		flags hook.Flags
		ok    bool
	}{
		{name: "plugin poll argv", args: []string{"opencode", "poll", "--session", "ses_1"},
			agent: "opencode", event: "poll", flags: hook.Flags{SessionID: "ses_1"}, ok: true},
		{name: "plugin status argv", args: []string{"opencode", "status", "--session", "ses_1", "--state", "busy"},
			agent: "opencode", event: "status", flags: hook.Flags{SessionID: "ses_1", State: "busy"}, ok: true},
		{name: "plugin end argv", args: []string{"opencode", "session-end", "--session", "ses_1"},
			agent: "opencode", event: "session-end", flags: hook.Flags{SessionID: "ses_1"}, ok: true},
		{name: "flags first", args: []string{"--session", "ses_1", "opencode", "poll"},
			agent: "opencode", event: "poll", flags: hook.Flags{SessionID: "ses_1"}, ok: true},
		{name: "equals form", args: []string{"opencode", "status", "--state=busy", "--session=ses_1"},
			agent: "opencode", event: "status", flags: hook.Flags{SessionID: "ses_1", State: "busy"}, ok: true},
		{name: "flagless hook thyself", args: []string{"codex", "stop"},
			agent: "codex", event: "stop", ok: true},
		{name: "missing event", args: []string{"opencode"}, ok: false},
		{name: "extra positional", args: []string{"opencode", "poll", "extra"}, ok: false},
		{name: "unknown flag", args: []string{"opencode", "poll", "--nope"}, ok: false},
	}
	for _, tc := range cases {
		agent, event, flags, ok := hookArgs(tc.args)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if agent != tc.agent || event != tc.event || flags != tc.flags {
			t.Errorf("%s: agent=%q event=%q flags=%+v, want %q %q %+v", tc.name, agent, event, flags, tc.agent, tc.event, tc.flags)
		}
	}
}

// TestCmdHookParsesPluginArgv pins the wiring itself: the plugin's exact argv must come out of
// cmdHook as the right agent/event for hook.Run, not just out of the parser in isolation.
func TestCmdHookParsesPluginArgv(t *testing.T) {
	var agent, event string
	var flags hook.Flags
	calls := 0
	orig := hookRun
	hookRun = func(_ context.Context, a, e string, _ io.Reader, _ io.Writer, _ func(ctx context.Context, op string, args, result any) error, _ func(string) string, f hook.Flags) error {
		agent, event, flags, calls = a, e, f, calls+1
		return nil
	}
	defer func() { hookRun = orig }()

	code := cmdHook([]string{"opencode", "status", "--session", "ses_1", "--state", "busy"})
	if code != 0 || calls != 1 {
		t.Fatalf("code=%d calls=%d, want 0/1", code, calls)
	}
	if agent != "opencode" || event != "status" || flags.SessionID != "ses_1" || flags.State != "busy" {
		t.Fatalf("hook.Run got agent=%q event=%q flags=%+v", agent, event, flags)
	}
}
