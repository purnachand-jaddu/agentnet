package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/setup"
)

// startFakeSocket opens a bare listener at paths.Socket(); it doesn't speak the wire protocol,
// which these tests don't need — they only care that ipc.Dial against it succeeds.
func startFakeSocket(t *testing.T) {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
}

func TestAutostartDialSpawnsWithoutInstalledService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))

	oldExec, oldRun := execCommand, runCommand
	var execCalled, runCalled bool
	execCommand = func(name string, arg ...string) *exec.Cmd {
		execCalled = true
		startFakeSocket(t)
		return exec.Command("true")
	}
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		runCalled = true
		return nil, nil
	}
	t.Cleanup(func() { execCommand, runCommand = oldExec, oldRun })

	c, err := autostartDial()
	if err != nil {
		t.Fatalf("autostartDial: %v", err)
	}
	c.Close()

	if !execCalled {
		t.Fatal("execCommand was never called")
	}
	if runCalled {
		t.Fatal("runCommand must not run without an installed service")
	}
}

func TestAutostartDialUsesInstalledServiceInsteadOfSpawning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))

	svcPath := setup.ServicePath(home, runtime.GOOS)
	if err := os.MkdirAll(filepath.Dir(svcPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcPath, []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldExec, oldRun := execCommand, runCommand
	var execCalled bool
	var runArgs []string
	execCommand = func(name string, arg ...string) *exec.Cmd {
		execCalled = true
		return exec.Command("true")
	}
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		runArgs = append([]string{name}, args...)
		startFakeSocket(t)
		return nil, nil
	}
	t.Cleanup(func() { execCommand, runCommand = oldExec, oldRun })

	c, err := autostartDial()
	if err != nil {
		t.Fatalf("autostartDial: %v", err)
	}
	c.Close()

	if execCalled {
		t.Fatal("execCommand (raw spawn) must not run when the service is installed")
	}
	if len(runArgs) == 0 {
		t.Fatal("runCommand (kickstart/start) was never called")
	}
	if runtime.GOOS == "darwin" {
		if runArgs[0] != "launchctl" || runArgs[1] != "kickstart" {
			t.Fatalf("runArgs = %v, want launchctl kickstart ...", runArgs)
		}
	} else {
		if runArgs[0] != "systemctl" || runArgs[2] != "start" {
			t.Fatalf("runArgs = %v, want systemctl --user start ...", runArgs)
		}
	}
}

// TestAutostartDialFallsBackToSpawnWhenServiceCannotStart checks that a service file existing
// but never answering (StartService errors, or nothing ever answers) still ends in a spawn,
// rather than leaving autostartDial permanently unreachable.
func TestAutostartDialFallsBackToSpawnWhenServiceCannotStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))

	svcPath := setup.ServicePath(home, runtime.GOOS)
	if err := os.MkdirAll(filepath.Dir(svcPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcPath, []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWait := serviceStartWait
	serviceStartWait = 50 * time.Millisecond // nobody ever answers; keep the test fast
	t.Cleanup(func() { serviceStartWait = oldWait })

	oldExec, oldRun := execCommand, runCommand
	var execCalled bool
	execCommand = func(name string, arg ...string) *exec.Cmd {
		execCalled = true
		startFakeSocket(t)
		return exec.Command("true")
	}
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("launchctl: No GUI session") // e.g. SSH without a GUI session
	}
	t.Cleanup(func() { execCommand, runCommand = oldExec, oldRun })

	c, err := autostartDial()
	if err != nil {
		t.Fatalf("autostartDial: %v", err)
	}
	c.Close()

	if !execCalled {
		t.Fatal("execCommand (fallback spawn) was never called")
	}
}

// TestAutostartDialSkipsServiceWhenAgentnetHomeIsCustom checks the service path is skipped
// entirely when AGENTNET_HOME isn't the default the installed service actually serves.
func TestAutostartDialSkipsServiceWhenAgentnetHomeIsCustom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", t.TempDir()) // not <home>/.agentnet

	svcPath := setup.ServicePath(home, runtime.GOOS)
	if err := os.MkdirAll(filepath.Dir(svcPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcPath, []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldExec, oldRun := execCommand, runCommand
	var execCalled, runCalled bool
	execCommand = func(name string, arg ...string) *exec.Cmd {
		execCalled = true
		startFakeSocket(t)
		return exec.Command("true")
	}
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		runCalled = true
		return nil, nil
	}
	t.Cleanup(func() { execCommand, runCommand = oldExec, oldRun })

	c, err := autostartDial()
	if err != nil {
		t.Fatalf("autostartDial: %v", err)
	}
	c.Close()

	if runCalled {
		t.Fatal("the service path must be skipped when AGENTNET_HOME is custom")
	}
	if !execCalled {
		t.Fatal("execCommand (spawn) was never called")
	}
}
