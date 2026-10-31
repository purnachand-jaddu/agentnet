// Package consent asks a person, never an agent, to approve an action. It slows agents down; it is not a security boundary.
package consent

import (
	"context"
	"errors"
	"os"
)

// ErrNoHuman means no person can be asked to approve: run this in your own terminal.
var ErrNoHuman = errors.New("needs a person to approve: run this in your own terminal")

// Approver asks a person to approve an action.
type Approver interface {
	Confirm(ctx context.Context, title, detail string) (bool, error)
	Choose(ctx context.Context, title, detail string, options []string) (string, error) // "" when cancelled
}

// isTerminal reports whether f is a character device; nil is never a terminal.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Default picks the approver: the system dialog on macOS; elsewhere a desktop dialog when a display
// and a dialog tool exist (never on Windows), a TTY prompt for a human terminal, else NoHuman.
func Default(goos string, getenv func(string) string, lookPath func(string) (string, error), in, out *os.File) Approver {
	if goos == "darwin" {
		return Dialog{Run: runOsascript}
	}
	if goos != "windows" && (getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != "") {
		if tool := linuxTool(lookPath); tool != "" {
			return LinuxDialog{Tool: tool, Run: runDialog}
		}
	}
	if isTerminal(in) && isTerminal(out) && !AgentEnv(getenv) {
		return TTY{In: in, Out: out}
	}
	return NoHuman{}
}

// NoHuman always refuses: there is no person to ask.
type NoHuman struct{}

func (NoHuman) Confirm(ctx context.Context, title, detail string) (bool, error) {
	return false, ErrNoHuman
}

func (NoHuman) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	return "", ErrNoHuman
}
