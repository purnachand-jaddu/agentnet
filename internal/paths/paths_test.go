package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEverythingUnderHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTNET_HOME", dir)
	if Home() != dir {
		t.Fatalf("Home() = %q", Home())
	}
	for _, p := range []string{Lock(), DB(), MachineKey(), FounderKey(), Config(), Log(), Audit()} {
		if !strings.HasPrefix(p, dir) {
			t.Errorf("%q is outside the state home", p)
		}
	}
}

func TestSocketFitsSunPath(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	t.Setenv("AGENTNET_HOME", long)
	if len(Socket()) > 100 {
		t.Fatalf("socket path %d bytes; macOS allows 104", len(Socket()))
	}
	t.Setenv("AGENTNET_HOME", "/tmp/a")
	if Socket() != "/tmp/a/agentnet.sock" {
		t.Fatalf("short home should keep the plain path, got %q", Socket())
	}
}

func TestEnsureCreatesPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	t.Setenv("AGENTNET_HOME", dir)
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
}
