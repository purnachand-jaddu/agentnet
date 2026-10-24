// Package paths names the files agentnet keeps in its state home.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

// Socket is the daemon's Unix socket; a short hashed path in the temp dir when Home is too long for sun_path.
func Socket() string {
	p := filepath.Join(Home(), "agentnet.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(Home()))
	return filepath.Join(os.TempDir(), "agentnet-"+hex.EncodeToString(sum[:6])+".sock")
}

func Lock() string       { return filepath.Join(Home(), "daemon.lock") }
func DB() string         { return filepath.Join(Home(), "agentnet.db") }
func MachineKey() string { return filepath.Join(Home(), "machine.key") }
func FounderKey() string { return filepath.Join(Home(), "founder.key") }
func Config() string     { return filepath.Join(Home(), "config.json") }
func Log() string        { return filepath.Join(Home(), "daemon.log") }
func Audit() string      { return filepath.Join(Home(), "audit.log") }

// Ensure creates the state folder, readable only by its owner.
func Ensure() error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return os.Chmod(Home(), 0o700)
}
