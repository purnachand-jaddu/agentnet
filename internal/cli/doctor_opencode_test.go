package cli

import (
	"strings"
	"testing"
)

// TestDoctorOpencodeWired runs the opencode agent line through its states: wired is ok, a missing
// plugin with registered MCP fails the report, and not-installed is n/a.
func TestDoctorOpencodeLines(t *testing.T) {
	c := healthyChecks()
	c.Agents = func() doctorAgents {
		return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: true, CodexFound: true, CodexConfig: true, CodexHooks: true,
			OpenCodeFound: true, OpenCodeMCP: true, OpenCodePlugin: true}
	}
	lines, code := runDoctor(c)
	out := render(lines)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "opencode: wired") {
		t.Fatalf("output:\n%s", out)
	}

	c.Agents = func() doctorAgents {
		return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: true, CodexFound: true, CodexConfig: true, CodexHooks: true,
			OpenCodeFound: true, OpenCodeMCP: true}
	}
	lines, code = runDoctor(c)
	out = render(lines)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "opencode: installed but plugin missing") {
		t.Fatalf("output:\n%s", out)
	}

	c.Agents = func() doctorAgents {
		return doctorAgents{CodexFound: true, CodexConfig: true, CodexHooks: true, OpenCodeFound: true, OpenCodeMCP: false}
	}
	lines, code = runDoctor(c)
	out = render(lines)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "opencode: installed but not wired") {
		t.Fatalf("output:\n%s", out)
	}
}

// TestDoctorOpencodeJSONCRequiresManualMCP pins the honesty rule: a jsonc home is never reported
// as wired — the config snippet there is a manual step doctor fails until the person does it.
func TestDoctorOpencodeJSONCRequiresManualMCP(t *testing.T) {
	c := healthyChecks()
	c.Agents = func() doctorAgents {
		return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: true, CodexFound: true, CodexConfig: true, CodexHooks: true,
			OpenCodeFound: true, OpenCodePlugin: true, OpenCodeJSONC: true}
	}
	lines, code := runDoctor(c)
	out := render(lines)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (a manual jsonc step remains)", code)
	}
	if !strings.Contains(out, "opencode.jsonc") {
		t.Fatalf("output must name the manual jsonc step:\n%s", out)
	}
}

// TestDoctorOpencodeNotInstalledIsNotApplicable keeps a machine without opencode at exit 0.
func TestDoctorOpencodeNotInstalledIsNotApplicable(t *testing.T) {
	c := healthyChecks()
	c.Agents = func() doctorAgents {
		return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: true, CodexFound: true, CodexConfig: true, CodexHooks: true}
	}
	lines, code := runDoctor(c)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (not-installed is n/a)", code)
	}
	if !strings.Contains(render(lines), "\u2013 opencode: not installed") {
		t.Fatalf("output:\n%s", render(lines))
	}
}
