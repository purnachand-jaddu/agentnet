package opencode

import (
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func TestMetaSessionIDReadsOpenCodeMetaKey(t *testing.T) {
	a := New()
	if id := a.MetaSessionID(map[string]any{"ai.opencode/sessionID": "ses_123"}); id != "ses_123" {
		t.Fatalf("meta session id = %q", id)
	}
	if id := a.MetaSessionID(map[string]any{"other": "x"}); id != "" {
		t.Fatalf("expected empty for unrelated meta, got %q", id)
	}
}

func TestEnvSessionIDReadsOpenCodeSessionID(t *testing.T) {
	a := New()
	if id := a.EnvSessionID(func(string) string { return "" }); id != "" {
		t.Fatalf("expected empty without env, got %q", id)
	}
	if id := a.EnvSessionID(func(k string) string { return "OPENCODE_SESSION_ID:" + k }); id != "OPENCODE_SESSION_ID:OPENCODE_SESSION_ID" {
		t.Fatalf("env session id = %q", id)
	}
}

func TestMultiplexedAndNames(t *testing.T) {
	a := New()
	if a.Name() != "opencode" || a.Short() != "opencode" {
		t.Fatalf("name/short = %q/%q", a.Name(), a.Short())
	}
	if !a.Multiplexed() {
		t.Fatal("openCode servers one MCP process across sessions; expected multiplexed")
	}
}

func TestChannelIsHookAfterHooksSeenPullBefore(t *testing.T) {
	a := New()
	s := adapter.Session{Key: "opencode:s1", Agent: "opencode", ID: "s1"}

	if ch := a.Channel(s, adapter.State{}); ch != adapter.ChannelPull {
		t.Fatalf("before hooks = %v, want pull", ch)
	}
	if ch := a.Channel(s, adapter.State{HookSeen: true}); ch != adapter.ChannelHook {
		t.Fatalf("after hooks = %v, want hook", ch)
	}
	if tp := a.Tier(s, adapter.State{HookSeen: true}); tp != adapter.Turn {
		t.Fatalf("tier after hooks = %v, want turn", tp)
	}
	if tp := a.Tier(s, adapter.State{}); tp != adapter.Pull {
		t.Fatalf("tier before hooks = %v, want pull", tp)
	}
}

func TestPushCannotPush(t *testing.T) {
	a := New()
	if err := a.Push(nil, adapter.Session{}, "hi"); err != adapter.ErrCannotPush {
		t.Fatalf("push = %v, want ErrCannotPush", err)
	}
}

func TestHookOutputPlainForPollAndStop(t *testing.T) {
	a := New()
	for _, ev := range []adapter.HookEvent{adapter.EventPoll, adapter.EventStop} {
		out, ok := a.HookOutput(ev, "hello from team")
		if !ok || out != "hello from team" {
			t.Fatalf("%v: out=%q ok=%v, want plain text true", ev, out, ok)
		}
	}
	for _, ev := range []adapter.HookEvent{adapter.EventStart, adapter.EventPrompt, adapter.EventTool, adapter.EventEnd} {
		if out, ok := a.HookOutput(ev, "x"); out != "" || ok {
			t.Fatalf("%v: expected no hook output, got %q ok=%v", ev, out, ok)
		}
	}
}

func TestAdapterSatisfiesInterface(t *testing.T) {
	var _ adapter.Adapter = New()
}
