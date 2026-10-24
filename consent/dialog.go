package consent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Dialog asks a person through a macOS system dialog (osascript).
type Dialog struct {
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// exitCoder matches *exec.ExitError: osascript exits 1 when the person cancels.
type exitCoder interface{ ExitCode() int }

// runOsascript is Dialog's real runner, shelling out to osascript.
func runOsascript(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// escapeAppleScript escapes backslash and double quote for a quoted AppleScript string literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// Confirm shows a Deny/Approve dialog that defaults to Deny and gives up after 300s.
func (d Dialog) Confirm(ctx context.Context, title, detail string) (bool, error) {
	script := `display dialog "` + escapeAppleScript(detail) + `" with title "` + escapeAppleScript(title) +
		`" buttons {"Deny", "Approve"} default button "Deny" cancel button "Deny" giving up after 300 with icon caution`
	out, err := d.Run(ctx, "osascript", "-e", script)
	if err != nil {
		var ec exitCoder
		if errors.As(err, &ec) && ec.ExitCode() == 1 {
			return false, nil // cancel button
		}
		return false, err
	}
	s := string(out)
	return strings.Contains(s, "button returned:Approve") && !strings.Contains(s, "gave up:true"), nil
}

// Choose shows a macOS list picker and returns the chosen option, or "" on cancel or a bogus result.
func (d Dialog) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	quoted := make([]string, len(options))
	for i, o := range options {
		quoted[i] = `"` + escapeAppleScript(o) + `"`
	}
	script := `choose from list {` + strings.Join(quoted, ", ") + `} with title "` + escapeAppleScript(title) +
		`" with prompt "` + escapeAppleScript(detail) + `"`
	out, err := d.Run(ctx, "osascript", "-e", script)
	if err != nil {
		var ec exitCoder
		if errors.As(err, &ec) && ec.ExitCode() == 1 {
			return "", nil // cancel button
		}
		return "", err
	}
	choice := strings.TrimSpace(string(out))
	for _, o := range options {
		if choice == o {
			return choice, nil
		}
	}
	return "", nil
}
