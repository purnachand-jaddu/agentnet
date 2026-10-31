package consent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
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
	a := Default("linux", env(map[string]string{"CLAUDECODE": "1"}), nil, nil)
	if _, err := a.Confirm(context.Background(), "t", "d"); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("err = %v", err)
	}
}

func TestMacAlwaysUsesDialog(t *testing.T) {
	if _, ok := Default("darwin", env(nil), nil, nil).(Dialog); !ok {
		t.Fatal("macOS should always ask through the system dialog")
	}
}

func TestDialog(t *testing.T) {
	var script string
	d := Dialog{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		script = args[len(args)-1]
		return []byte("button returned:Approve\n"), nil
	}}
	ok, err := d.Confirm(context.Background(), `Join "team"`, "code 123-456-789")
	if err != nil || !ok || !strings.Contains(script, `default button "Deny"`) || !strings.Contains(script, `\"team\"`) {
		t.Fatalf("ok %v err %v script %q", ok, err, script)
	}
	d.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("button returned:Deny\n"), nil }
	if ok, _ := d.Confirm(context.Background(), "t", "d"); ok {
		t.Fatal("Deny approved")
	}
}

func TestDialogGaveUp(t *testing.T) {
	d := Dialog{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("button returned:Approve, gave up:true\n"), nil
	}}
	if ok, err := d.Confirm(context.Background(), "t", "d"); err != nil || ok {
		t.Fatalf("gave up should not approve: ok %v err %v", ok, err)
	}
}

// fakeExitError mimics *exec.ExitError's ExitCode() for a failed osascript run.
type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return "exit status" }
func (e fakeExitError) ExitCode() int { return e.code }

// osascriptFails returns a runner that exits 1 printing stderr, as osascript does for every AppleScript error.
func osascriptFails(stderr string) func(context.Context, string, ...string) ([]byte, error) {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(stderr), fakeExitError{code: 1}
	}
}

func TestDialogCancelIsNotAnError(t *testing.T) {
	d := Dialog{Run: osascriptFails("0:153: execution error: User canceled. (-128)\n")}
	ok, err := d.Confirm(context.Background(), "t", "d")
	if ok || err != nil {
		t.Fatalf("cancel should be (false, nil), got (%v, %v)", ok, err)
	}
	if got, err := d.Choose(context.Background(), "t", "d", []string{"a"}); got != "" || err != nil {
		t.Fatalf("cancelled choose: got %q, %v", got, err)
	}
}

// With no GUI session (e.g. over SSH) osascript fails with -1713: that is "no person to ask", not a denial.
func TestDialogNoGUIIsNoHuman(t *testing.T) {
	d := Dialog{Run: osascriptFails("0:153: execution error: No user interaction allowed. (-1713)\n")}
	if ok, err := d.Confirm(context.Background(), "t", "d"); ok || !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Confirm = %v, %v; want false, ErrNoHuman", ok, err)
	}
	if _, err := d.Choose(context.Background(), "t", "d", []string{"a"}); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("Choose err = %v, want ErrNoHuman", err)
	}
}

// Any other AppleScript error is reported with osascript's own message, never read as a denial.
func TestDialogOtherAppleScriptErrorCarriesStderr(t *testing.T) {
	d := Dialog{Run: osascriptFails("0:10: syntax error: Expected end of line. (-2741)\n")}
	ok, err := d.Confirm(context.Background(), "t", "d")
	if ok || err == nil || !strings.Contains(err.Error(), "Expected end of line. (-2741)") {
		t.Fatalf("Confirm = %v, %v; want an error carrying stderr", ok, err)
	}
}

func TestDialogOtherRunErrorIsReturned(t *testing.T) {
	wantErr := errors.New("osascript not found")
	d := Dialog{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, wantErr
	}}
	ok, err := d.Confirm(context.Background(), "t", "d")
	if ok || !errors.Is(err, wantErr) {
		t.Fatalf("got (%v, %v), want (false, %v)", ok, err, wantErr)
	}
}

func TestDialogChoose(t *testing.T) {
	d := Dialog{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		script := args[len(args)-1]
		if !strings.Contains(script, `choose from list {"a", "b"}`) {
			t.Fatalf("script missing list: %q", script)
		}
		return []byte("a\n"), nil
	}}
	got, err := d.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "a" {
		t.Fatalf("got %q, %v", got, err)
	}

	d.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("false\n"), nil }
	got, err = d.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "" {
		t.Fatalf("cancelled choose: got %q, %v", got, err)
	}

	// Output not one of the given options must not be trusted.
	d.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("c\n"), nil }
	got, err = d.Choose(context.Background(), "t", "d", []string{"a", "b"})
	if err != nil || got != "" {
		t.Fatalf("bogus choice: got %q, %v", got, err)
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

func TestDefaultNonTerminalFallsBackToNoHuman(t *testing.T) {
	// nil files are not terminals, so even without an agent env this is NoHuman.
	if _, ok := Default("linux", env(nil), nil, nil).(NoHuman); !ok {
		t.Fatal("expected NoHuman when in/out are not terminals")
	}
}
