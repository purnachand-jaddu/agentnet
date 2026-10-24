package adapter_test

import (
	"testing"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
)

func TestRegistryGet(t *testing.T) {
	codex := generic.New()
	r := adapter.NewRegistry(codex)
	got, ok := r.Get("mcp")
	if !ok || got != codex {
		t.Fatalf("Get(mcp) = %v, %v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope) found something")
	}
}

// withClientNames wraps a generic adapter to also implement adapter.ClientNamer.
type withClientNames struct {
	*generic.Adapter
	clients []string
}

func (w *withClientNames) ClientNames() []string { return w.clients }

func TestFromClient(t *testing.T) {
	codex := &withClientNames{Adapter: generic.WithEnv("codex", "CODEX_SESSION_ID"), clients: []string{"codex-mcp-client"}}
	claude := generic.WithEnv("claude-code", "CLAUDE_SESSION_ID")
	r := adapter.NewRegistry(codex, claude)

	got, ok := r.FromClient("codex-mcp-client")
	if !ok || got.Name() != "codex" {
		t.Fatalf("FromClient(codex-mcp-client) = %v, %v", got, ok)
	}
	got, ok = r.FromClient("claude-code")
	if !ok || got.Name() != "claude-code" {
		t.Fatalf("FromClient(claude-code) = %v, %v", got, ok)
	}
	if _, ok := r.FromClient("claude-ai"); ok {
		t.Fatal("claude-ai matched the claude-code adapter")
	}
}

func TestFromEnv(t *testing.T) {
	codex := generic.WithEnv("codex", "CODEX_SESSION_ID")
	claude := generic.WithEnv("claude-code", "CLAUDE_SESSION_ID")
	r := adapter.NewRegistry(codex, claude)

	getenv := func(k string) string {
		if k == "CLAUDE_SESSION_ID" {
			return "c1"
		}
		return ""
	}
	got, ok := r.FromEnv(getenv)
	if !ok || got.Name() != "claude-code" {
		t.Fatalf("FromEnv = %v, %v", got, ok)
	}
}
