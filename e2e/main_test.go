package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binPath is the real agentnet binary, built once for every test in this package; buildErr is set
// if the build itself failed. Shared by local_test.go (untagged) and network_test.go (-tags e2e).
var (
	binPath  string
	buildErr error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mkdtemp:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "agentnet")
	buildErr = buildAgentnet(binPath)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// buildAgentnet builds ./cmd/agentnet to out with the e2e tag, resolving the module root via `go env GOMOD`.
// The tag only adds a dead-code approver seam (internal/cli/testapprove_e2e.go) unless
// AGENTNET_TEST_APPROVE=yes is set, so it's safe to build this way even for the untagged local_test.go.
func buildAgentnet(out string) error {
	modOut, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return fmt.Errorf("go env GOMOD: %w", err)
	}
	modRoot := filepath.Dir(strings.TrimSpace(string(modOut)))

	build := exec.Command("go", "build", "-tags", "e2e", "-o", out, "./cmd/agentnet")
	build.Dir = modRoot
	if cout, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, cout)
	}
	return nil
}
