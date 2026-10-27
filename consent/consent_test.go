package consent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/zenity"
)

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func TestAgentEnv(t *testing.T) {
	if !AgentEnv(env(map[string]string{"CODEX_THREAD_ID": "x"})) || AgentEnv(env(nil)) {
		t.Fatal("AgentEnv wrong")
	}
}

func TestTTYNeedsYes(t *testing.T) {
	for in, want := range map[string]bool{"yes\n": true, "y\n": false, "\n": false} {
		ok, err := TTY{In: strings.NewReader(in), Out: &bytes.Buffer{}}.Confirm(context.Background(), "Join?", "detail")
		if err != nil || ok != want {
			t.Errorf("%q → %v, %v", in, ok, err)
		}
	}
}

func TestLinuxAgentGetsNoHuman(t *testing.T) {
	a := Default("linux", env(map[string]string{"CLAUDECODE": "1"}), func() bool { return false }, nil, nil)
	if _, err := a.Confirm(context.Background(), "t", "d"); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("err = %v", err)
	}
}

func TestMacAlwaysUsesDialog(t *testing.T) {
	// macOS must ask through a system dialog even when the dialog backend reports absent.
	if _, ok := Default("darwin", env(nil), func() bool { return false }, nil, nil).(Dialog); !ok {
		t.Fatal("macOS should always ask through the system dialog")
	}
}

// A Linux desktop shows a system dialog too, even while an agent runs the command.
func TestLinuxDialogWhenGUI(t *testing.T) {
	if _, ok := Default("linux", env(map[string]string{"DISPLAY": ":0"}), func() bool { return true }, nil, nil).(Dialog); !ok {
		t.Fatal("expected a Dialog whenever a dialog backend and a display exist")
	}
}

// An SSH session on a desktop box with zenity installed but no display falls back to the prompt.
func TestLinuxDialogNeedsDisplay(t *testing.T) {
	term, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	a := Default("linux", env(nil), func() bool { return true }, term, term)
	if _, ok := a.(TTY); !ok {
		t.Fatalf("expected the TTY fallback without a display, got %T", a)
	}
}

// Windows dialogs need no display variable.
func TestWindowsDialogWithoutDisplay(t *testing.T) {
	if _, ok := Default("windows", env(nil), func() bool { return true }, nil, nil).(Dialog); !ok {
		t.Fatal("expected a Dialog on Windows")
	}
}

func TestDialog(t *testing.T) {
	d := Dialog{Pick: func(_ context.Context, _, _ string, options []string) (string, error) {
		if len(options) != 2 || options[0] != "Deny" || options[1] != "Approve" {
			t.Errorf("confirm must offer Deny first (the backend's silent Enter default), got %v", options)
		}
		return "Approve", nil
	}}
	if ok, err := d.Confirm(context.Background(), "t", "d"); err != nil || !ok {
		t.Fatalf("got (%v, %v), want (true, nil)", ok, err)
	}
	d.Pick = func(context.Context, string, string, []string) (string, error) { return "Deny", nil }
	if ok, err := d.Confirm(context.Background(), "t", "d"); ok || err != nil {
		t.Fatalf("Deny approved: (%v, %v)", ok, err)
	}
}

// Any pick result that is not the literal "Approve" string is a denial: junk,
// doubled or padded results can never approve.
func TestDialogConfirmRejectsUnvalidatedResult(t *testing.T) {
	for _, returned := range []string{"", "button returned:Approve", "APPROVE", "Approve ", "Approve\nApprove", "ApproveApprove", "true"} {
		d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) { return returned, nil }}
		if ok, err := d.Confirm(context.Background(), "t", "d"); ok || err != nil {
			t.Fatalf("pick %q approved: (%v, %v)", returned, ok, err)
		}
	}
}

func TestDialogCancelIsNotAnError(t *testing.T) {
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "", zenity.ErrCanceled
	}}
	ok, err := d.Confirm(context.Background(), "t", "d")
	if ok || err != nil {
		t.Fatalf("cancel should be (false, nil), got (%v, %v)", ok, err)
	}
}

func TestDialogGaveUp(t *testing.T) {
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "", context.DeadlineExceeded
	}}
	if ok, err := d.Confirm(context.Background(), "t", "d"); err != nil || ok {
		t.Fatalf("gave up should not approve: ok %v err %v", ok, err)
	}
}

// Confirm must give up on its own after five minutes, like the old "giving up after
// 300" dialog, because callers pass context.Background().
func TestDialogConfirmsExpireAfterFiveMinutes(t *testing.T) {
	var got context.Context
	d := Dialog{Pick: func(ctx context.Context, _, _ string, _ []string) (string, error) {
		got = ctx
		return "Deny", nil
	}}
	if _, err := d.Confirm(context.Background(), "t", "d"); err != nil {
		t.Fatal(err)
	}
	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("pick context has no deadline; a stale dialog would never expire")
	}
	if left := time.Until(deadline); left < 4*time.Minute || left > 5*time.Minute+10*time.Second {
		t.Fatalf("deadline in %v, want about five minutes", left)
	}
}

func TestDialogNoGUIIsNoHuman(t *testing.T) {
	// With no GUI session (e.g. over SSH) osascript fails with "-1713 errorNumber":
	// that is "no person to ask", not a denial.
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "", errors.New("Error -1713: No user interaction allowed")
	}}
	if ok, err := d.Confirm(context.Background(), "t", "d"); ok || !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Confirm = %v, %v; want false, ErrNoHuman", ok, err)
	}
	if _, err := d.Choose(context.Background(), "t", "d", []string{"a"}); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Choose err = %v, want ErrNoHuman", err)
	}
}

// Any other backend error is reported with its own message, never read as a denial.
func TestDialogOtherErrorCarriesMessage(t *testing.T) {
	wantErr := errors.New("syntax error: Expected end of line. (-2741)")
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "", wantErr
	}}
	ok, err := d.Confirm(context.Background(), "t", "d")
	if ok || !errors.Is(err, wantErr) {
		t.Fatalf("Confirm = %v, %v; want (false, %v)", ok, err, wantErr)
	}
}

// An error arriving with a misleading pick result still fails closed.
func TestDialogPickErrorWinsOverResult(t *testing.T) {
	wantErr := errors.New("backend hiccup")
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "Approve", wantErr
	}}
	if ok, err := d.Confirm(context.Background(), "t", "d"); ok || !errors.Is(err, wantErr) {
		t.Fatalf("Confirm = %v, %v; want (false, %v)", ok, err, wantErr)
	}
}

// A cancelled context is an error, not a silent denial.
func TestDialogContextCancelIsReported(t *testing.T) {
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) {
		return "", context.Canceled
	}}
	if _, err := d.Confirm(context.Background(), "t", "d"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Confirm err = %v, want context.Canceled", err)
	}
}

func TestDialogChoose(t *testing.T) {
	d := Dialog{Pick: func(context.Context, string, string, []string) (string, error) { return "a", nil }}
	got, err := d.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "a" {
		t.Fatalf("got %q, %v", got, err)
	}

	d.Pick = func(context.Context, string, string, []string) (string, error) { return "", zenity.ErrCanceled }
	if got, err = d.Choose(context.Background(), "t", "d", []string{"a", "b"}); err != nil || got != "" {
		t.Fatalf("cancelled choose: got %q, %v", got, err)
	}

	// Output not one of the given options must not be trusted.
	d.Pick = func(context.Context, string, string, []string) (string, error) { return "c", nil }
	if got, err = d.Choose(context.Background(), "t", "d", []string{"a", "b"}); err != nil || got != "" {
		t.Fatalf("bogus choice: got %q, %v", got, err)
	}

	d.Pick = func(context.Context, string, string, []string) (string, error) { return "", context.DeadlineExceeded }
	if got, err = d.Choose(context.Background(), "t", "d", []string{"a", "b"}); err != nil || got != "" {
		t.Fatalf("gave-up choose: got %q, %v", got, err)
	}
}

func TestTTYChoose(t *testing.T) {
	out := &bytes.Buffer{}
	got, err := TTY{In: strings.NewReader("2\n"), Out: out}.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "b" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "1") || !strings.Contains(out.String(), "2") {
		t.Fatalf("expected numbered list, got %q", out.String())
	}

	got, err = TTY{In: strings.NewReader("9\n"), Out: &bytes.Buffer{}}.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "" {
		t.Fatalf("out of range: got %q, %v", got, err)
	}

	got, err = TTY{In: strings.NewReader("nope\n"), Out: &bytes.Buffer{}}.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "" {
		t.Fatalf("non-numeric: got %q, %v", got, err)
	}
}

func TestNoHuman(t *testing.T) {
	var n NoHuman
	if _, err := n.Confirm(context.Background(), "t", "d"); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Confirm err = %v", err)
	}
	if choice, err := n.Choose(context.Background(), "t", "d", []string{"a"}); choice != "" || !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Choose = %q, %v", choice, err)
	}
}

// A Linux desktop shows a system dialog even when an agent set agent env vars:
// that is the point — the dialog reaches the person the agent cannot reach.
func TestLinuxDialogBeatsAgentEnv(t *testing.T) {
	get := env(map[string]string{"CLAUDECODE": "1", "DISPLAY": ":0"})
	if _, ok := Default("linux", get, func() bool { return true }, nil, nil).(Dialog); !ok {
		t.Fatal("expected a Dialog when a backend and display exist, agent env or not")
	}
}

func TestLinuxWaylandOnlyDialog(t *testing.T) {
	get := env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"})
	if _, ok := Default("linux", get, func() bool { return true }, nil, nil).(Dialog); !ok {
		t.Fatal("expected a Dialog under Wayland")
	}
}

func TestDefaultNoBackendNonTerminalIsNoHuman(t *testing.T) {
	// Without a dialog backend, nil files are not terminals, so even without an agent
	// env this is NoHuman.
	if _, ok := Default("linux", env(nil), func() bool { return false }, nil, nil).(NoHuman); !ok {
		t.Fatal("expected NoHuman when in/out are not terminals")
	}
}

func TestDefaultTTYWhenTerminal(t *testing.T) {
	// A real terminal with no agent env and no dialog backend gets the TTY prompt.
	// /dev/null is a character device, which isTerminal treats as a terminal.
	term, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	a := Default("linux", env(nil), func() bool { return false }, term, term)
	if _, ok := a.(TTY); !ok {
		t.Fatalf("expected TTY, got %T", a)
	}
}
