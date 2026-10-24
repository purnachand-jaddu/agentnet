package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/consent"
)

func TestApproveNoHumanExitsNonZeroNoChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, consent.NoHuman{})

	if code := Main([]string{"approve", "sam"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json exists or stat failed: %v", err)
	}
}

func TestRefuseNoHumanExitsNonZeroNoChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, consent.NoHuman{})

	if code := Main([]string{"refuse", "sam"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json exists or stat failed: %v", err)
	}
}

func TestResumeNoHumanExitsNonZeroNoChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, consent.NoHuman{})

	if code := Main([]string{"resume"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json exists or stat failed: %v", err)
	}
}

func TestPauseNeedsNoConsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, consent.NoHuman{}) // pause must never ask

	if code := Main([]string{"pause"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	cfg, err := config.Load(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Paused {
		t.Fatal("Paused = false, want true")
	}
}

func TestResumeWithConsentClearsPaused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, consent.NoHuman{})
	if code := Main([]string{"pause"}, "v0"); code != 0 {
		t.Fatalf("pause: code = %d", code)
	}

	withApprover(t, fakeApprover{})
	if code := Main([]string{"resume"}, "v0"); code != 0 {
		t.Fatalf("resume: code = %d, want 0", code)
	}
	cfg, err := config.Load(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paused {
		t.Fatal("Paused = true, want false")
	}
}
