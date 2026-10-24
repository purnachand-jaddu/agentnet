package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/setup"
)

// autostartRetry bounds how often autostartDial polls the socket while waiting for a start.
const autostartRetry = 100 * time.Millisecond

// autostartWait and serviceStartWait bound how long autostartDial waits for a freshly spawned
// daemon, or a kickstarted/started service, to answer.
const (
	autostartWait    = 3 * time.Second
	serviceStartWait = 5 * time.Second
)

// execCommand builds the detached daemon's command; a var so tests can intercept it.
var execCommand = exec.Command

// runCommand runs an external command for the service-takeover path; a var so tests can fake it.
var runCommand setup.Runner = setup.Exec

// autostartDial dials the daemon's socket. When agentnet's background service is installed, a
// missed dial asks it to (re)start instead of spawning a second, non-service daemon that would
// race it for the socket; otherwise it spawns one detached, as before.
func autostartDial() (*ipc.Client, error) {
	if c, err := ipc.Dial(paths.Socket()); err == nil {
		return c, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	installed, err := setup.ServiceInstalled(home, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	if installed {
		if err := setup.StartService(runtime.GOOS, runCommand); err != nil {
			return nil, err
		}
		return waitForDial(serviceStartWait)
	}

	if err := spawnDaemon(); err != nil {
		return nil, err
	}
	return waitForDial(autostartWait)
}

// waitForDial polls the daemon's socket until it answers, or returns an error after timeout.
func waitForDial(timeout time.Duration) (*ipc.Client, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(paths.Socket()); err == nil {
			return c, nil
		}
		time.Sleep(autostartRetry)
	}
	return nil, fmt.Errorf("agentnet: daemon did not start within %s", timeout)
}

// spawnDaemon starts this same executable as `agentnet daemon`, detached, logging to paths.Log().
func spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()

	cmd := execCommand(exe, "daemon")
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
