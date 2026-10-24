package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
)

func TestPushRunsCodexQueueWithArgv(t *testing.T) {
	var got []string
	a := New(func(_ context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	})
	if err := a.Push(context.Background(), adapter.Session{ID: "th-1"}, `x"; rm -rf ~`); err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "queue", "--thread", "th-1", "--message=" + `x"; rm -rf ~`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q", got)
	}
}

func TestMidTurnUsesHooksIdleUsesQueue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	started := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	os.WriteFile(p, []byte(started), 0o600)
	a := New(nil)
	a.lookPath = func(string) (string, error) { return "/usr/bin/codex", nil }
	if c := a.Channel(adapter.Session{Transcript: p}, adapter.State{}); c != adapter.ChannelHook {
		t.Fatalf("mid-turn channel = %v", c)
	}
	os.WriteFile(p, []byte(started+`{"type":"event_msg","payload":{"type":"task_complete"}}`+"\n"), 0o600)
	if c := a.Channel(adapter.Session{Transcript: p}, adapter.State{}); c != adapter.ChannelCommand {
		t.Fatalf("idle channel = %v", c)
	}
}

func TestMetaSessionID(t *testing.T) {
	if id := New(nil).MetaSessionID(map[string]any{"threadId": "t9"}); id != "t9" {
		t.Fatalf("got %q", id)
	}
}
