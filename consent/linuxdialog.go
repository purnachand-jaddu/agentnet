package consent

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// linuxTools are the zenity-compatible dialog programs LinuxDialog can drive, in order of preference.
var linuxTools = []string{"zenity", "qarma", "matedialog"}

// dialogGiveUp is how long a dialog waits for an answer before it is killed and read as a denial.
const dialogGiveUp = 5 * time.Minute

// LinuxDialog asks a person through a zenity-compatible list dialog (zenity, qarma or matedialog).
type LinuxDialog struct {
	Tool string
	Run  func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// linuxTool returns the path of the first dialog program lookPath finds, or "".
func linuxTool(lookPath func(string) (string, error)) string {
	for _, name := range linuxTools {
		if p, err := lookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// runDialog is LinuxDialog's real runner; the context deadline kills the dialog.
func runDialog(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// escapeDialogText escapes s for --text, which is g_strcompress'd and then parsed as Pango markup.
func escapeDialogText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), `\`, `\\`)
}

// Confirm approves only when the person picks Approve; it gives up and denies after five minutes.
func (d LinuxDialog) Confirm(ctx context.Context, title, detail string) (bool, error) {
	choice, err := d.pick(ctx, title, detail, []string{"Deny", "Approve"})
	return choice == "Approve", err
}

// Choose returns the chosen option, or "" on cancel, giving up or a result not among options.
func (d LinuxDialog) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	for _, o := range options {
		if strings.HasPrefix(o, "-") {
			return "", fmt.Errorf("consent: dialog option %q starts with '-'", o)
		}
	}
	choice, err := d.pick(ctx, title, detail, options)
	if err != nil || !slices.Contains(options, choice) {
		return "", err
	}
	return choice, nil
}

// pick runs the list dialog and returns its trimmed stdout, "" for cancel or giving up.
func (d LinuxDialog) pick(ctx context.Context, title, detail string, options []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialogGiveUp)
	defer cancel()
	args := append([]string{"--list", "--hide-header", "--title=" + title, "--text=" + escapeDialogText(detail), "--column="}, options...)
	stdout, stderr, err := d.Run(ctx, d.Tool, args...)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", nil
	}
	if err != nil {
		return "", dialogErr(d.Tool, stderr, err)
	}
	return strings.TrimSpace(string(stdout)), nil
}

// dialogErr maps a failed dialog run: no usable display is ErrNoHuman, other exit 1 (cancel) is nil, else an error with stderr.
func dialogErr(tool string, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(stderr))
	lower := strings.ToLower(msg)
	for _, s := range []string{"cannot open display", "could not connect to display", "failed to open display"} {
		if strings.Contains(lower, s) {
			return ErrNoHuman
		}
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("%s: %w: %s", tool, err, msg)
}
