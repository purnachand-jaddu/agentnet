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

// Default picks the approver for this environment: macOS always uses the system
// dialog; elsewhere a system dialog when one can be shown (backend plus, on Linux,
// an actual display; see dialogs), then a TTY prompt when stdin/stderr are terminals
// and no agent env is set, else NoHuman.
func Default(goos string, getenv func(string) string, dialogs func() bool, in, out *os.File) Approver {
	if dialogs() && hasDisplay(goos, getenv) || goos == "darwin" {
		return Dialog{Pick: pickZenity}
	}
	if isTerminal(in) && isTerminal(out) && !AgentEnv(getenv) {
		return TTY{In: in, Out: out}
	}
	return NoHuman{}
}

// hasDisplay reports whether the environment can put a dialog on screen: darwin and
// windows dialogs need no display variable, while Linux needs DISPLAY or WAYLAND_DISPLAY.
func hasDisplay(goos string, getenv func(string) string) bool {
	switch goos {
	case "darwin", "windows":
		return true
	}
	return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}

// NoHuman always refuses: there is no person to ask.
type NoHuman struct{}

func (NoHuman) Confirm(ctx context.Context, title, detail string) (bool, error) {
	return false, ErrNoHuman
}

func (NoHuman) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	return "", ErrNoHuman
}
