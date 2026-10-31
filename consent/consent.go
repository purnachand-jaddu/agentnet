// Package consent asks a person, never an agent, to approve an action. It slows agents down; it is not a security boundary.
package consent

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrNoHuman means no person can be asked to approve: run this in your own terminal.
var ErrNoHuman = errors.New("needs a person to approve: run this in your own terminal")

// Approver asks a person to approve an action.
type Approver interface {
	Confirm(ctx context.Context, title, detail string) (bool, error)
	Choose(ctx context.Context, title, detail string, options []string) (string, error) // "" when cancelled
}

// dialogGiveUp is how long a dialog waits for an answer before it is killed and read as a denial.
const dialogGiveUp = 5 * time.Minute

// dialogBody is the text a dialog shows: detail, or title when detail is empty.
func dialogBody(title, detail string) string {
	if detail == "" {
		return title
	}
	return detail
}

// isTerminal reports whether f is a character device; nil is never a terminal.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Default picks the approver: the system dialog on macOS; elsewhere a TTY prompt for a human terminal,
// then a desktop dialog when a display and a dialog tool exist (never on Windows), else NoHuman.
func Default(goos string, getenv func(string) string, lookPath func(string) (string, error), in, out *os.File) Approver {
	if goos == "darwin" {
		return Dialog{Run: runOsascript}
	}
	if isTerminal(in) && isTerminal(out) && !AgentEnv(getenv) {
		return TTY{In: in, Out: out}
	}
	if goos != "windows" && (getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != "") {
		if tool := linuxTool(lookPath); tool != "" {
			return LinuxDialog{Tool: tool, Run: runDialog}
		}
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
