// Package paths names the files agentnet keeps in its state home.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Home is the state folder: $AGENTNET_HOME, else ~/.agentnet.
func Home() string {
	if h := os.Getenv("AGENTNET_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		u = "."
	}
	return filepath.Join(u, ".agentnet")
}

// Socket is the daemon's Unix socket; a short hashed path in a per-user temp dir when Home is too long for sun_path.
func Socket() string {
	p := filepath.Join(Home(), "agentnet.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(Home()))
	sockDir := filepath.Join(os.TempDir(), "agentnet-"+strconv.Itoa(os.Getuid()))
	return filepath.Join(sockDir, hex.EncodeToString(sum[:12])+".sock")
}

// SocketDir ensures the fallback socket directory exists, is private (0700), and is owned by the current user.
func SocketDir() error {
	p := filepath.Join(Home(), "agentnet.sock")
	if len(p) <= 100 {
		return nil
	}
	dir := filepath.Join(os.TempDir(), "agentnet-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: is a symlink", dir)
	}
	stat := st.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s: owned by uid %d, not %d", dir, stat.Uid, os.Getuid())
	}
	if st.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s: mode %03o, want 0700", dir, st.Mode().Perm())
	}
	return nil
}

func Lock() string       { return filepath.Join(Home(), "daemon.lock") }
func DB() string         { return filepath.Join(Home(), "agentnet.db") }
func MachineKey() string { return filepath.Join(Home(), "machine.key") }
func FounderKey() string { return filepath.Join(Home(), "founder.key") }
func Config() string     { return filepath.Join(Home(), "config.json") }
func Log() string        { return filepath.Join(Home(), "daemon.log") }
func Audit() string      { return filepath.Join(Home(), "audit.log") }

// Ensure creates the state folder, readable only by its owner, and the fallback socket dir if needed.
func Ensure() error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(Home(), 0o700); err != nil {
		return err
	}
	return SocketDir()
}
