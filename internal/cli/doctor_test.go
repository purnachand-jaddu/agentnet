package cli

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
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
			return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: true, CodexFound: true, CodexConfig: true, CodexHooks: true}
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

// TestDoctorClockSkewWarning covers the explicit decision: a warning when |clock diff| > 2 minutes,
// shown as ⚠ and never failing the exit code by itself.
func TestDoctorClockSkewWarning(t *testing.T) {
	c := healthyChecks()
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: true, LastSeen: time.Now(), ClockDiff: 5 * time.Minute}}, nil
	}
	lines, code := runDoctor(c)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (a clock-skew warning alone must not fail doctor)\n%s", code, render(lines))
	}
	out := render(lines)
	if !strings.Contains(out, "⚠ peer sam@xps") || !strings.Contains(out, "clock skew exceeds 2m") {
		t.Fatalf("output missing clock skew warning line:\n%s", out)
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

// TestDoctorNeverSeenPeerStillShowsTimeRefusals proves the counter doesn't disappear precisely when
// it matters most: a peer whose clock skew is bad enough that it has never once been accepted.
func TestDoctorNeverSeenPeerStillShowsTimeRefusals(t *testing.T) {
	c := healthyChecks()
	c.Peers = func() ([]daemon.PeerInfo, error) {
		return []daemon.PeerInfo{{Person: "sam", Machine: "xps", Online: false, TimeRefusals: 3}}, nil // zero LastSeen
	}
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	out := render(lines)
	if !strings.Contains(out, "peer sam@xps: never seen") || !strings.Contains(out, "clock skew suspected (3 refusals)") {
		t.Fatalf("output missing never-seen clock-skew diagnostic:\n%s", out)
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
	c.Agents = func() doctorAgents { return doctorAgents{CodexFound: true, CodexConfig: true, CodexHooks: true} } // claude not found
	lines, code := runDoctor(c)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (not-installed is n/a, not a failure)\n%s", code, render(lines))
	}
	if !strings.Contains(render(lines), "\u2013 Claude Code: not installed") {
		t.Fatalf("output:\n%s", render(lines))
	}
}

// TestDoctorAgentPartiallyWiredReportsWhichPart proves a half-wired agent (MCP but no hooks, or vice
// versa) fails and names exactly what's missing, not a generic "not wired".
func TestDoctorAgentPartiallyWiredReportsWhichPart(t *testing.T) {
	c := healthyChecks()
	c.Agents = func() doctorAgents {
		return doctorAgents{ClaudeFound: true, ClaudeMCP: true, ClaudeHooks: false, CodexFound: true, CodexConfig: false, CodexHooks: true}
	}
	lines, code := runDoctor(c)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	out := render(lines)
	if !strings.Contains(out, "Claude Code: installed but hooks missing") {
		t.Fatalf("output missing Claude hooks-missing line:\n%s", out)
	}
	if !strings.Contains(out, "Codex: installed but MCP not registered") {
		t.Fatalf("output missing Codex MCP-missing line:\n%s", out)
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

// TestDoctorDoesNotModifyDatabase runs doctor's REAL (uninjected) Team check against a real store
// and proves the database file's mode, mtime and size are unchanged: doctor never writes to it.
func TestDoctorDoesNotModifyDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatal("init failed")
	}

	dbPath := paths.DB()
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	withIO(t) // exit code doesn't matter here: no daemon is running, so some lines are ✗ regardless
	Main([]string{"doctor"}, "v0")

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("mode changed: %v -> %v", before.Mode(), after.Mode())
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
	if before.Size() != after.Size() {
		t.Fatalf("size changed: %d -> %d", before.Size(), after.Size())
	}
}

// TestDoctorMissingDatabaseIsNotInANetwork proves a never-created db reads as "not in a network",
// not an error, and doctor still never creates the file.
func TestDoctorMissingDatabaseIsNotInANetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	Main([]string{"doctor"}, "v0")
	if _, err := os.Stat(paths.DB()); !os.IsNotExist(err) {
		t.Fatalf("doctor should never create the db; stat err = %v", err)
	}
}
