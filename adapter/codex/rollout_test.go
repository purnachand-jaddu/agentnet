package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnRunningMissingFile(t *testing.T) {
	running, known := TurnRunning(filepath.Join(t.TempDir(), "missing.jsonl"))
	if running || known {
		t.Fatalf("missing file: running=%v, known=%v (want false, false)", running, known)
	}
}

func TestTurnRunningOver256KiBEndingAborted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	// Create a file with content > 256 KiB and ending in turn_aborted.
	content := strings.Repeat(`{"type":"event_msg","payload":{"type":"other"}}`, 6000)
	content += "\n" + `{"type":"event_msg","payload":{"type":"turn_aborted"}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	running, known := TurnRunning(p)
	if running || !known {
		t.Fatalf("after turn_aborted: running=%v, known=%v (want false, true)", running, known)
	}
}

func TestTurnRunningBoundaryStraddle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	// Create content where the last "task_started" line straddles a 256 KiB boundary.
	// First, fill to near 256 KiB with a repeated pattern.
	line := `{"type":"event_msg","payload":{"type":"other"}}` + "\n"
	var content string
	for len(content) < 256*1024-100 {
		content += line
	}
	// Add the boundary-straddling line.
	content += `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	content += line // Add some more content after it.
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	running, known := TurnRunning(p)
	if !running || !known {
		t.Fatalf("boundary-straddling task_started: running=%v, known=%v (want true, true)", running, known)
	}
}

func TestTurnRunningTaskStarted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	running, known := TurnRunning(p)
	if !running || !known {
		t.Fatalf("task_started: running=%v, known=%v (want true, true)", running, known)
	}
}

func TestTurnRunningTaskComplete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	running, known := TurnRunning(p)
	if running || !known {
		t.Fatalf("task_complete: running=%v, known=%v (want false, true)", running, known)
	}
}

func TestTurnRunningNoMarkers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"type":"event_msg","payload":{"type":"other"}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	running, known := TurnRunning(p)
	if running || !known {
		t.Fatalf("no markers: running=%v, known=%v (want false, true)", running, known)
	}
}
