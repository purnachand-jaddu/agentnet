// Package proc finds the real agent process behind the shell wrappers that run hooks.
package proc

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// shellNames are command names AgentPID treats as a wrapper to walk past, not the agent itself.
var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true}

// AgentPID walks up from start while the process's comm is a shell, stopping at pid<=1, after 8
// hops, or on a ps error; it returns the last non-shell pid seen, or start if it never found one.
func AgentPID(start int, ps func(pid int) (ppid int, comm string, err error)) int {
	pid := start
	for hops := 0; hops < 8; hops++ {
		if pid <= 1 {
			break
		}
		ppid, comm, err := ps(pid)
		if err != nil {
			break
		}
		if !isShell(comm) {
			return pid
		}
		pid = ppid
	}
	return start
}

// isShell reports whether comm (its basename, leading '-' stripped for a login shell) names a shell.
func isShell(comm string) bool {
	name := strings.TrimPrefix(filepath.Base(comm), "-")
	return shellNames[name]
}

// PS runs `ps -o ppid=,comm= -p <pid>`, which works the same on macOS and Linux, with a 2s timeout.
func PS(pid int) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("proc: unexpected ps output %q", out)
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", err
	}
	return ppid, strings.Join(fields[1:], " "), nil
}
