package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// autostartRetry and autostartWait bound how long autostartDial waits for a freshly spawned daemon.
const (
	autostartRetry = 100 * time.Millisecond
	autostartWait  = 3 * time.Second
)

// autostartDial dials the daemon's socket, starting it detached first if nothing answers.
func autostartDial() (*ipc.Client, error) {
	if c, err := ipc.Dial(paths.Socket()); err == nil {
		return c, nil
	}
	if err := spawnDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(autostartWait)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(paths.Socket()); err == nil {
			return c, nil
		}
		time.Sleep(autostartRetry)
	}
	return nil, fmt.Errorf("agentnet: daemon did not start within %s", autostartWait)
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

	cmd := exec.Command(exe, "daemon")
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
