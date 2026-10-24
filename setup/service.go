package setup

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// launchAgentLabel is the launchd job label (and plist basename, minus ".plist").
const launchAgentLabel = "dev.agentnet.daemon"

// shutdownWait and startWait bound how long the service step waits for the old daemon to let go
// of the socket, and for the newly (re)started one to pick it back up.
const (
	shutdownWait = 3 * time.Second
	startWait    = 5 * time.Second
)

// serviceStep installs and starts agentnet's background daemon as the OS's per-user service.
func serviceStep(home, exe string, run Runner, goos string, binDirs []string) Step {
	if goos == "darwin" {
		return macServiceStep(home, exe, run, binDirs)
	}
	return linuxServiceStep(home, exe, run, binDirs)
}

// takeOverRunningDaemon asks any daemon already answering on paths.Socket() to shut down, and
// waits for it to let go, so the service we're about to (re)start becomes the one true runner
// instead of losing the flock to it and sitting in backoff. A no-op if nothing is listening.
func takeOverRunningDaemon() {
	c, err := ipc.Dial(paths.Socket())
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	c.Call(ctx, "shutdown", nil, nil) // best effort; the poll below is what actually matters
	cancel()
	c.Close()
	waitForSocket(shutdownWait, false)
}

// waitForSocket polls paths.Socket() until dialing it succeeds (want=true) or fails (want=false),
// or timeout elapses; it reports whether that state was reached.
func waitForSocket(timeout time.Duration, want bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		c, err := ipc.Dial(paths.Socket())
		answers := err == nil
		if c != nil {
			c.Close()
		}
		if answers == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// servicePATH is the PATH the service runs with: binDirs (where claude/codex were found) first,
// then the usual system bin directories, deduped. The service must be the normal runner, so an
// autostarted daemon doesn't inherit an agent's sandbox.
func servicePATH(binDirs []string) string {
	dirs := append(append([]string{}, binDirs...),
		"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, ":")
}

// escapeXML renders s safe for use inside plist XML text content.
func escapeXML(s string) string {
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// plistXML renders the launchd job's property list.
func plistXML(exe string, binDirs []string) string {
	env := fmt.Sprintf("<key>PATH</key>\n        <string>%s</string>", escapeXML(servicePATH(binDirs)))
	if h := os.Getenv("AGENTNET_HOME"); h != "" {
		env += fmt.Sprintf("\n        <key>AGENTNET_HOME</key>\n        <string>%s</string>", escapeXML(h))
	}
	logPath := escapeXML(paths.Log())
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>--service</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		%s
	</dict>
</dict>
</plist>
`, launchAgentLabel, escapeXML(exe), logPath, logPath, env)
}

// macServiceStep installs dev.agentnet.daemon.plist as a launchd LaunchAgent.
func macServiceStep(home, exe string, run Runner, binDirs []string) Step {
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	target := "gui/" + strconv.Itoa(os.Getuid())
	service := target + "/" + launchAgentLabel
	return Step{
		Name: "Background service (launchd)",
		Apply: func() error {
			if err := paths.Ensure(); err != nil {
				return err
			}
			takeOverRunningDaemon()

			if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(plistPath, []byte(plistXML(exe, binDirs)), 0o644); err != nil {
				return err
			}

			run(context.Background(), "launchctl", "bootout", service) // ignore: fine if it wasn't loaded
			if _, err := run(context.Background(), "launchctl", "bootstrap", target, plistPath); err != nil {
				return err
			}

			if !waitForSocket(startWait, true) {
				return fmt.Errorf("service did not start; see %s", paths.Log())
			}
			return nil
		},
		Remove: func() error {
			run(context.Background(), "launchctl", "bootout", service)
			if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
		Check: func() (bool, error) {
			_, err := os.Stat(plistPath)
			return err == nil, nil
		},
	}
}

// systemdQuoteIfNeeded double-quotes s for a systemd unit's Exec= syntax if it contains anything
// outside [A-Za-z0-9_./-].
func systemdQuoteIfNeeded(s string) string {
	for _, r := range s {
		if !isUnquotedSafe(r) {
			return `"` + tomlEscape(s) + `"` // systemd's "..." escaping matches \ and " doubling
		}
	}
	return s
}

// systemdUnit renders agentnet's systemd user unit.
func systemdUnit(exe string, binDirs []string) string {
	env := "Environment=PATH=" + servicePATH(binDirs)
	if h := os.Getenv("AGENTNET_HOME"); h != "" {
		env += "\nEnvironment=AGENTNET_HOME=" + h
	}
	return fmt.Sprintf(`[Unit]
Description=agentnet daemon

[Service]
ExecStart=%s daemon --service
Restart=always
%s

[Install]
WantedBy=default.target
`, systemdQuoteIfNeeded(exe), env)
}

// linuxServiceStep installs agentnet.service as a systemd user unit.
func linuxServiceStep(home, exe string, run Runner, binDirs []string) Step {
	unitPath := filepath.Join(home, ".config", "systemd", "user", "agentnet.service")
	return Step{
		Name: "Background service (systemd)",
		Apply: func() error {
			if err := paths.Ensure(); err != nil {
				return err
			}
			takeOverRunningDaemon()

			if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(unitPath, []byte(systemdUnit(exe, binDirs)), 0o644); err != nil {
				return err
			}

			if _, err := run(context.Background(), "systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			if _, err := run(context.Background(), "systemctl", "--user", "enable", "agentnet"); err != nil {
				return err
			}
			if _, err := run(context.Background(), "systemctl", "--user", "restart", "agentnet"); err != nil {
				return err
			}

			if !waitForSocket(startWait, true) {
				return fmt.Errorf("service did not start; see %s", paths.Log())
			}
			return nil
		},
		Remove: func() error {
			run(context.Background(), "systemctl", "--user", "disable", "--now", "agentnet")
			if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
		Check: func() (bool, error) {
			_, err := os.Stat(unitPath)
			return err == nil, nil
		},
	}
}
