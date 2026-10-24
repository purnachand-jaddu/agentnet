package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/daemon"
)

// withDoctorChecks injects c into newDoctorChecks for the duration of t, restoring the original after.
func withDoctorChecks(t *testing.T, c doctorChecks) {
	t.Helper()
	old := newDoctorChecks
	newDoctorChecks = func() doctorChecks { return c }
	t.Cleanup(func() { newDoctorChecks = old })
}

// healthyChecks is a fully-wired, all-green fake environment: no sockets, processes or files touched.
func healthyChecks() doctorChecks {
	return doctorChecks{
		Status: func() (daemon.StatusResult, error) {
			return daemon.StatusResult{PID: 123, Service: true, Addrs: []string{"lan://10.0.0.1:7777"}}, nil
		},
		ServiceInstalled: func() (bool, error) { return true, nil },
		Team: func() (doctorTeamInfo, error) {
			return doctorTeamInfo{InTeam: true, TeamCode: "ABCD-EFGH-JKLM", Person: "alex", Machine: "mbp",
				Admin: true, HasFounderKey: true, FounderKeyPath: "/home/alex/.agentnet/founder.key"}, nil
		},
		Peers: func() ([]daemon.PeerInfo, error) {
			return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: true, LastSeen: time.Now().Add(-5 * time.Second), ClockDiff: time.Second}}, nil
		},
		Agents: func() doctorAgents {
			return doctorAgents{ClaudeFound: true, ClaudeOK: true, CodexFound: true, CodexOK: true}
		},
		Folders: func() ([]string, error) { return []string{"/home/alex/src"}, nil },
		GOOS:    "linux",
	}
}

// render joins lines back into the text runDoctor would print, for substring assertions.
func render(lines []line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.mark + " " + l.text + "\n")
	}
	return b.String()
}

func TestDoctorAllHealthyExitsZero(t *testing.T) {
	lines, code := runDoctor(healthyChecks())
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, render(lines))
	}
	out := render(lines)
	for _, want := range []string{"\u2713 daemon: running (pid 123)", "\u2713 service", "\u2713 team: ABCD-EFGH-JKLM", "\u2713 peer sam@xps", "\u2713 listeners:", "\u2713 Claude Code: wired", "\u2713 Codex: wired", "\u2713 folders:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorDaemonNotRunning(t *testing.T) {
	c := healthyChecks()
	c.Status = func() (daemon.StatusResult, error) { return daemon.StatusResult{}, errors.New("dial: no such file") }
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, render(lines))
	}
	if !strings.Contains(render(lines), "\u2717 daemon: not running") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

// TestDoctorServiceInstalledButNotRunningIt covers the explicit decision: service installed, but the
// answering daemon isn't the service -> ✗ re-run setup.
func TestDoctorServiceInstalledButNotRunningIt(t *testing.T) {
	c := healthyChecks()
	c.Status = func() (daemon.StatusResult, error) { return daemon.StatusResult{PID: 1, Service: false}, nil }
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(render(lines), "\u2717 service: installed but not running the daemon; re-run `agentnet setup`") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

func TestDoctorNotInTeamShowsNotApplicable(t *testing.T) {
	c := healthyChecks()
	c.Team = func() (doctorTeamInfo, error) { return doctorTeamInfo{}, nil }
	lines, code := runDoctor(c)
	out := render(lines)
	if !strings.Contains(out, "\u2013 team: not in a network") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "\u2013 peers:") && !strings.Contains(out, "\u2013 listeners: not in a network") {
		t.Fatalf("peers/listeners should be n/a without a team:\n%s", out)
	}
	_ = code
}

// TestDoctorClockSkewWarning covers the explicit decision: warn when |clock diff| > 2 minutes.
func TestDoctorClockSkewWarning(t *testing.T) {
	c := healthyChecks()
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: true, LastSeen: time.Now(), ClockDiff: 5 * time.Minute}}, nil
	}
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	out := render(lines)
	if !strings.Contains(out, "clock skew exceeds 2m") {
		t.Fatalf("output missing clock skew warning:\n%s", out)
	}
}

func TestDoctorOfflinePeer(t *testing.T) {
	c := healthyChecks()
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: false, LastSeen: time.Now().Add(-10 * time.Minute)}}, nil
	}
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(render(lines), "\u2717 peer sam@xps: offline") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

// TestDoctorMacOSHintWhenNoPeerEverSeen covers the macOS-only local-network-permission hint.
func TestDoctorMacOSHintWhenNoPeerEverSeen(t *testing.T) {
	c := healthyChecks()
	c.GOOS = "darwin"
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: false}}, nil // zero LastSeen: never seen
	}
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(render(lines), "macOS may be blocking local network access") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

// TestDoctorMacOSHintSkippedOnOtherOS proves the hint is OS-gated, not just condition-gated.
func TestDoctorMacOSHintSkippedOnOtherOS(t *testing.T) {
	c := healthyChecks()
	c.GOOS = "linux"
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: false}}, nil
	}
	lines, _ := runDoctor(c)
	if strings.Contains(render(lines), "macOS may be blocking") {
		t.Fatalf("mac hint should not appear on linux:\n%s", render(lines))
	}
}

func TestDoctorNoFoldersIsAFailure(t *testing.T) {
	c := healthyChecks()
	c.Folders = func() ([]string, error) { return nil, nil }
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(render(lines), "run `agentnet folder add <path>`") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

func TestDoctorAgentNotInstalledIsNotApplicable(t *testing.T) {
	c := healthyChecks()
	c.Agents = func() doctorAgents { return doctorAgents{CodexFound: true, CodexOK: true} } // claude not found
	lines, code := runDoctor(c)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (not-installed is n/a, not a failure)\n%s", code, render(lines))
	}
	if !strings.Contains(render(lines), "\u2013 Claude Code: not installed") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

// TestCmdDoctorPrintsAndReturnsCode wires cmdDoctor itself (not just runDoctor) through an injected
// environment, proving the Main-facing command prints the lines and surfaces the exit code.
func TestCmdDoctorPrintsAndReturnsCode(t *testing.T) {
	withDoctorChecks(t, healthyChecks())
	out, _ := withIO(t)
	if code := Main([]string{"doctor"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "daemon: running") {
		t.Fatalf("stdout = %q", out.String())
	}
}
