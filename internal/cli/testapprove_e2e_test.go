package cli

import (
	"bufio"
	"os"
	"testing"
)

// TestTestApproverHasE2EBuildTag ensures testapprove_e2e.go keeps its build tag as the file's first
// line, so a plain build (no -tags e2e) never links in the auto-approving approver.
func TestTestApproverHasE2EBuildTag(t *testing.T) {
	f, err := os.Open("testapprove_e2e.go")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("testapprove_e2e.go is empty")
	}
	if got := sc.Text(); got != "//go:build e2e" {
		t.Fatalf("first line = %q, want %q", got, "//go:build e2e")
	}
}
