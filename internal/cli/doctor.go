package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/setup"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

// clockSkewWarn is how much a peer's clock may differ from this machine's before doctor flags it.
const clockSkewWarn = 2 * time.Minute

// doctorTeamInfo is this machine's local team facts, for the "team" check.
type doctorTeamInfo struct {
	InTeam         bool
	TeamCode       string
	Person         string
	Machine        string
	Admin          bool
	FounderKeyPath string
	HasFounderKey  bool
}

// doctorAgents is whether Claude Code, Codex and opencode are installed, and whether their MCP
// registration and lifecycle hooks (or, for opencode, its plugin) are each wired, from setup's own
// read-only checks. OpenCodeJSONC means the config file is one agentnet can't edit.
type doctorAgents struct {
	ClaudeFound, ClaudeMCP, ClaudeHooks        bool
	CodexFound, CodexConfig, CodexHooks        bool
	OpenCodeFound, OpenCodeMCP, OpenCodePlugin bool
	OpenCodeJSONC                              bool
}

// doctorChecks is every call doctor makes to the outside world, as funcs, so tests can fake all of
// it: no real socket, process or filesystem access happens when a test supplies its own.
type doctorChecks struct {
	Status           func() (daemon.StatusResult, error)
	ServiceInstalled func() (bool, error)
	Team             func() (doctorTeamInfo, error)
	Peers            func() ([]daemon.PeerInfo, error)
	Agents           func() doctorAgents
	Folders          func() ([]string, error)
	GOOS             string
}

// newDoctorChecks builds doctorChecks against the real daemon, store and OS; tests override this var.
var newDoctorChecks = func() doctorChecks {
	return doctorChecks{
		Status:           doctorStatus,
		ServiceInstalled: doctorServiceInstalled,
		Team:             doctorTeamFromStore,
		Peers:            doctorPeers,
		Agents:           doctorAgentsFromSetup,
		Folders:          doctorFolders,
		GOOS:             runtime.GOOS,
	}
}

func doctorStatus() (daemon.StatusResult, error) {
	c, err := ipc.Dial(paths.Socket())
	if err != nil {
		return daemon.StatusResult{}, err
	}
	defer c.Close()
	var res daemon.StatusResult
	err = c.Call(context.Background(), "status", nil, &res)
	return res, err
}

func doctorServiceInstalled() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	return setup.ServiceInstalled(home, runtime.GOOS)
}

func doctorTeamFromStore() (doctorTeamInfo, error) {
	if _, err := os.Stat(paths.DB()); err != nil {
		return doctorTeamInfo{}, nil // no db yet: not in a network, and never create one by looking
	}
	st, err := sqlite.OpenReadOnly(paths.DB())
	if err != nil {
		return doctorTeamInfo{}, err
	}
	defer st.Close()
	tm, err := team.Load(st)
	if err != nil {
		return doctorTeamInfo{}, err
	}
	if tm == nil {
		return doctorTeamInfo{}, nil
	}
	info := doctorTeamInfo{InTeam: true, TeamCode: identity.TeamCode(tm.Founder), Person: tm.Self.Person,
		Machine: tm.Self.Machine, Admin: tm.Self.Admin}
	if _, err := os.Stat(paths.FounderKey()); err == nil {
		info.HasFounderKey, info.FounderKeyPath = true, paths.FounderKey()
	}
	return info, nil
}

func doctorPeers() ([]daemon.PeerInfo, error) {
	c, err := ipc.Dial(paths.Socket())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var res daemon.PeersResult
	if err := c.Call(context.Background(), "peers", nil, &res); err != nil {
		return nil, err
	}
	return res.Peers, nil
}

func doctorAgentsFromSetup() doctorAgents {
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorAgents{}
	}
	exe, err := os.Executable()
	if err != nil {
		return doctorAgents{}
	}
	var a doctorAgents
	if _, err := exec.LookPath("claude"); err == nil {
		a.ClaudeFound = true
		a.ClaudeMCP, a.ClaudeHooks, _ = setup.ClaudeWired(home, exe)
	}
	_, codexBinErr := exec.LookPath("codex")
	_, codexDirErr := os.Stat(filepath.Join(home, ".codex"))
	if codexBinErr == nil || codexDirErr == nil {
		a.CodexFound = true
		a.CodexConfig, a.CodexHooks, _ = setup.CodexWired(home, exe)
	}

	if _, err := exec.LookPath("opencode"); err == nil || opencodeDirExists(home) {
		a.OpenCodeFound = true
		a.OpenCodeMCP, a.OpenCodePlugin, a.OpenCodeJSONC, _ = setup.OpenCodeWired(home, exe)
	}
	return a
}

// opencodeDirExists reports whether OpenCode's config directory exists.
func opencodeDirExists(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".config", "opencode"))
	return err == nil
}

func doctorFolders() ([]string, error) {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return nil, err
	}
	return cfg.Folders, nil
}

// line is one doctor check's printed result.
type line struct {
	mark string // "✓" "✗" "–"
	text string
}

func okLine(text string) line   { return line{"✓", text} }
func badLine(text string) line  { return line{"✗", text} }
func naLine(text string) line   { return line{"–", text} }
func warnLine(text string) line { return line{"⚠", text} } // flagged, but never fails the exit code

// cmdDoctor runs `agentnet doctor`: a read-only health report, no consent needed.
func cmdDoctor(args []string) int {
	lines, code := runDoctor(newDoctorChecks())
	for _, l := range lines {
		fmt.Fprintf(stdout, "%s %s\n", l.mark, l.text)
	}
	return code
}

// runDoctor runs every check against c and returns the printed lines plus the exit code (1 if any ✗).
func runDoctor(c doctorChecks) ([]line, int) {
	var lines []line
	status, daemonErr := c.Status()
	lines = append(lines, daemonLine(status, daemonErr))
	lines = append(lines, serviceLine(c, status, daemonErr))

	teamInfo, err := c.Team()
	lines = append(lines, teamLine(teamInfo, err))

	lines = append(lines, peerLines(c, teamInfo)...)
	lines = append(lines, listenerLine(status, daemonErr, teamInfo))
	lines = append(lines, agentLines(c.Agents())...)
	lines = append(lines, folderLines(c)...)
	if l := macHint(c, teamInfo); l != nil {
		lines = append(lines, *l)
	}

	code := 0
	for _, l := range lines {
		if l.mark == "✗" {
			code = 1
		}
	}
	return lines, code
}

func daemonLine(st daemon.StatusResult, err error) line {
	if err != nil {
		return badLine(fmt.Sprintf("daemon: not running (%v); run `agentnet daemon` or `agentnet setup`", err))
	}
	return okLine(fmt.Sprintf("daemon: running (pid %d)", st.PID))
}

func serviceLine(c doctorChecks, st daemon.StatusResult, daemonErr error) line {
	installed, err := c.ServiceInstalled()
	if err != nil {
		return badLine(fmt.Sprintf("service: %v", err))
	}
	if !installed {
		return naLine("service: not installed (run `agentnet setup` to install it)")
	}
	if daemonErr != nil || !st.Service {
		return badLine("service: installed but not running the daemon; re-run `agentnet setup`")
	}
	return okLine("service: installed and running")
}

func teamLine(t doctorTeamInfo, err error) line {
	if err != nil {
		return badLine(fmt.Sprintf("team: %v", err))
	}
	if !t.InTeam {
		return naLine("team: not in a network (run `agentnet init` or `agentnet join`)")
	}
	role := "member"
	if t.Admin {
		role = "admin"
	}
	text := fmt.Sprintf("team: %s, %s@%s (%s)", t.TeamCode, t.Person, t.Machine, role)
	if t.HasFounderKey {
		text += fmt.Sprintf(", founder key at %s", t.FounderKeyPath)
	}
	return okLine(text)
}

func peerLines(c doctorChecks, t doctorTeamInfo) []line {
	if !t.InTeam {
		return nil
	}
	peers, err := c.Peers()
	if err != nil {
		return []line{badLine(fmt.Sprintf("peers: %v", err))}
	}
	if len(peers) == 0 {
		return []line{naLine("peers: no other members yet")}
	}
	now := time.Now()
	out := make([]line, len(peers))
	for i, p := range peers {
		out[i] = peerLine(p, now)
	}
	return out
}

// timeRefusalSuffix is the diagnostic appended whenever a peer has any authenticated time-window
// refusal: it already verified signature, so this is suspected real clock skew, not a replay.
func timeRefusalSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", clock skew suspected (%d refusal%s)", n, pluralS(n))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func peerLine(p daemon.PeerInfo, now time.Time) line {
	who := fmt.Sprintf("%s@%s", p.Person, p.Machine)
	if p.LastSeen.IsZero() {
		return badLine(fmt.Sprintf("peer %s: never seen; check that machine's network or daemon%s", who, timeRefusalSuffix(p.TimeRefusals)))
	}
	age := now.Sub(p.LastSeen).Round(time.Second)
	status := "offline"
	if p.Online {
		status = "online"
	}
	text := fmt.Sprintf("peer %s: %s, last seen %s ago, clock diff %s%s",
		who, status, age, p.ClockDiff.Round(time.Second), timeRefusalSuffix(p.TimeRefusals))
	skew := p.ClockDiff
	if skew < 0 {
		skew = -skew
	}
	switch {
	case !p.Online:
		return badLine(text + " — check network/daemon on that machine")
	case skew > clockSkewWarn:
		return warnLine(text + " — clock skew exceeds 2m; check this machine's clock")
	default:
		return okLine(text)
	}
}

func listenerLine(st daemon.StatusResult, daemonErr error, t doctorTeamInfo) line {
	if daemonErr != nil {
		return naLine("listeners: unknown (daemon not running)")
	}
	if !t.InTeam {
		return naLine("listeners: not in a network")
	}
	if len(st.Addrs) == 0 {
		return badLine("listeners: none; check network/firewall")
	}
	return okLine("listeners: " + strings.Join(st.Addrs, ", "))
}

func agentLines(a doctorAgents) []line {
	return []line{
		agentLine("Claude Code", a.ClaudeFound, a.ClaudeMCP, a.ClaudeHooks),
		agentLine("Codex", a.CodexFound, a.CodexConfig, a.CodexHooks),
		opencodeLine(a),
	}
}

// opencodeLine reports which part of the wiring (the MCP registration or the plugin) is missing.
func opencodeLine(a doctorAgents) line {
	if !a.OpenCodeFound {
		return naLine("opencode: not installed")
	}
	switch {
	case a.OpenCodeMCP && a.OpenCodePlugin:
		return okLine("opencode: wired")
	case a.OpenCodeJSONC && !a.OpenCodeMCP:
		return badLine("opencode: plugin " + wired(a.OpenCodePlugin) + ", but the MCP snippet must be hand-added to opencode.jsonc (`agentnet setup --dry-run` prints it)")
	case !a.OpenCodeMCP && !a.OpenCodePlugin:
		return badLine("opencode: installed but not wired (MCP and plugin both missing); re-run `agentnet setup`")
	case a.OpenCodeMCP:
		return badLine("opencode: installed but plugin missing; re-run `agentnet setup`")
	default:
		return badLine("opencode: installed but MCP not registered; re-run `agentnet setup`")
	}
}

// wired is the word for whether a piece is in place, for lines that compose it into longer text.
func wired(v bool) string {
	if v {
		return "wired"
	}
	return "missing"
}

// agentLine reports which part of the wiring (MCP registration, hooks, or both) is missing.
func agentLine(name string, found, configOK, hooksOK bool) line {
	if !found {
		return naLine(name + ": not installed")
	}
	switch {
	case configOK && hooksOK:
		return okLine(name + ": wired")
	case !configOK && !hooksOK:
		return badLine(name + ": installed but not wired (MCP and hooks both missing); re-run `agentnet setup`")
	case !configOK:
		return badLine(name + ": installed but MCP not registered; re-run `agentnet setup`")
	default:
		return badLine(name + ": installed but hooks missing; re-run `agentnet setup`")
	}
}

func folderLines(c doctorChecks) []line {
	folders, err := c.Folders()
	if err != nil {
		return []line{badLine(fmt.Sprintf("folders: %v", err))}
	}
	if len(folders) == 0 {
		return []line{badLine("folders: none opted in; run `agentnet folder add <path>`")}
	}
	return []line{okLine("folders: " + strings.Join(folders, ", "))}
}

// macHint adds a macOS-only line when no peer has ever been seen and the team has other members:
// a common symptom of macOS blocking local network access for an unsigned build.
func macHint(c doctorChecks, t doctorTeamInfo) *line {
	if c.GOOS != "darwin" || !t.InTeam {
		return nil
	}
	peers, err := c.Peers()
	if err != nil || len(peers) == 0 {
		return nil
	}
	for _, p := range peers {
		if !p.LastSeen.IsZero() {
			return nil // some peer has been seen
		}
	}
	l := badLine("macOS may be blocking local network access: System Settings → Privacy & Security → Local Network")
	return &l
}
