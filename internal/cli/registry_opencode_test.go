package cli

import (
	"testing"
)

// TestProductionRegistryResolvesOpencode pins the shared registry both daemons and MCP servers
// build from: opencode must resolve by its adapter name AND by its MCP client name (its binary
// declares clientInfo "opencode"), or the daemon would reject registrations the MCP server makes.
func TestProductionRegistryResolvesOpencode(t *testing.T) {
	r := adapters()
	if _, ok := r.Get("opencode"); !ok {
		t.Fatal(`adapters() lacks Get("opencode")`)
	}
	if _, ok := r.FromClient("opencode"); !ok {
		t.Fatal(`adapters() lacks FromClient("opencode")`)
	}
}
