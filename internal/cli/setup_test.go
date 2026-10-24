package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purnachand-jaddu/agentnet/consent"
)

// assertHomeUntouched fails t if home has anything in it besides the pre-existing entries in before.
func assertHomeUntouched(t *testing.T, home string, before []os.DirEntry) {
	t.Helper()
	after, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("home dir changed: before %v, after %v", before, after)
	}
}

func TestSetupDryRunChangesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))
	// No claude/codex on PATH, so Plan's Check() never shells out to a real agent binary.
	t.Setenv("PATH", t.TempDir())
	out, _ := withIO(t)

	before, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"setup", "--dry-run"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got := out.String(); got == "" {
		t.Fatal("dry-run printed nothing")
	}
	assertHomeUntouched(t, home, before)
}

func TestSetupNoHumanAbortsWithNoChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))
	// No claude/codex on PATH: this must abort before touching anything regardless.
	t.Setenv("PATH", t.TempDir())
	withIO(t)

	oldApprover := newApprover
	newApprover = func() consent.Approver { return consent.NoHuman{} }
	t.Cleanup(func() { newApprover = oldApprover })

	before, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"setup"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero on consent denial")
	}
	assertHomeUntouched(t, home, before)
}

// TestSetupRemoveOmitsFolderStep checks --remove's step list never offers to opt the cwd in —
// there's nothing for --remove to undo there.
func TestSetupRemoveOmitsFolderStep(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTNET_HOME", filepath.Join(home, ".agentnet"))
	t.Setenv("PATH", t.TempDir())

	out, _ := withIO(t)
	if code := Main([]string{"setup", "--dry-run"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Opt this folder in") {
		t.Fatalf("apply's dry-run should list the folder step: %q", out.String())
	}

	out, _ = withIO(t)
	if code := Main([]string{"setup", "--remove", "--dry-run"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "Opt this folder in") {
		t.Fatalf("--remove should omit the folder step: %q", out.String())
	}
}
