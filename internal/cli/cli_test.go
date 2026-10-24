package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/purnachand-jaddu/agentnet/consent"
)

// withIO redirects stdout/stderr to buffers for the duration of fn, restoring the originals after.
func withIO(t *testing.T) (out, errOut *bytes.Buffer) {
	t.Helper()
	oldOut, oldErr := stdout, stderr
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	stdout, stderr = out, errOut
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return
}

func TestMainVersion(t *testing.T) {
	out, _ := withIO(t)
	if code := Main([]string{"version"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got := out.String(); got != "v0\n" {
		t.Fatalf("stdout = %q, want %q", got, "v0\n")
	}
}

func TestMainUnknownCommand(t *testing.T) {
	withIO(t)
	if code := Main([]string{"bogus"}, "v0"); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestMainNoArgs(t *testing.T) {
	withIO(t)
	if code := Main(nil, "v0"); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestFolderAddNoHumanLeavesConfigUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)

	oldApprover := newApprover
	newApprover = func() consent.Approver { return consent.NoHuman{} }
	t.Cleanup(func() { newApprover = oldApprover })

	dir := t.TempDir()
	if code := Main([]string{"folder", "add", dir}, "v0"); code == 0 {
		t.Fatalf("code = 0, want non-zero")
	}

	cfgPath := filepath.Join(home, "config.json")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config.json exists or stat failed: %v", err)
	}
}
