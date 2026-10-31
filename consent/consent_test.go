package consent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
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

// lookPathFinds returns a lookPath that finds only the named programs.
func lookPathFinds(names ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		if slices.Contains(names, name) {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestDefault(t *testing.T) {
	term, err := os.OpenFile(os.DevNull, os.O_RDWR, 0) // a character device, so isTerminal says yes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	agent, display := map[string]string{"CLAUDECODE": "1"}, map[string]string{"DISPLAY": ":0"}
	agentDisplay := map[string]string{"CLAUDECODE": "1", "DISPLAY": ":0"}
	agentWayland := map[string]string{"CLAUDECODE": "1", "WAYLAND_DISPLAY": "wayland-0"}
	for _, c := range []struct {
		name     string
		goos     string
		env      map[string]string
		terminal bool
		tools    []string
		want     string
	}{
		{"darwin", "darwin", agent, false, nil, "consent.Dialog"},
		{"linux human terminal", "linux", nil, true, []string{"zenity"}, "consent.TTY"},
		{"linux human terminal display tool", "linux", display, true, []string{"zenity"}, "consent.TTY"},
		{"linux agent display tool", "linux", agentDisplay, false, []string{"zenity"}, "consent.LinuxDialog"},
		{"linux agent wayland tool", "linux", agentWayland, true, []string{"qarma"}, "consent.LinuxDialog"},
		{"linux no agent no terminal display tool", "linux", display, false, []string{"matedialog"}, "consent.LinuxDialog"},
		{"freebsd agent display tool", "freebsd", agentDisplay, false, []string{"zenity"}, "consent.LinuxDialog"},
		{"linux agent display no tool", "linux", agentDisplay, false, nil, "consent.NoHuman"},
		{"linux agent no display", "linux", agent, true, []string{"zenity"}, "consent.NoHuman"},
		{"linux no terminal no display", "linux", nil, false, []string{"zenity"}, "consent.NoHuman"},
		{"windows agent", "windows", agentDisplay, false, []string{"zenity"}, "consent.NoHuman"},
		{"windows human terminal", "windows", nil, true, []string{"zenity"}, "consent.TTY"},
	} {
		in, out := (*os.File)(nil), (*os.File)(nil)
		if c.terminal {
			in, out = term, term
		}
		if got := fmt.Sprintf("%T", Default(c.goos, env(c.env), lookPathFinds(c.tools...), in, out)); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDefaultLinuxDialogUsesFoundTool(t *testing.T) {
	a := Default("linux", env(map[string]string{"DISPLAY": ":0"}), lookPathFinds("qarma"), nil, nil)
	if d, ok := a.(LinuxDialog); !ok || d.Tool != "/usr/bin/qarma" || d.Run == nil {
		t.Fatalf("got %#v", a)
	}
}

func TestLinuxAgentGetsNoHuman(t *testing.T) {
	a := Default("linux", env(map[string]string{"CLAUDECODE": "1"}), lookPathFinds(), nil, nil)
	if _, err := a.Confirm(context.Background(), "t", "d"); !errors.Is(err, ErrNoHuman) {
		t.Fatalf("err = %v", err)
	}
}

func TestMacAlwaysUsesDialog(t *testing.T) {
	if _, ok := Default("darwin", env(nil), lookPathFinds(), nil, nil).(Dialog); !ok {
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

func TestDialogEmptyDetailShowsTitle(t *testing.T) {
	var script string
	d := Dialog{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		script = args[len(args)-1]
		return []byte("a\n"), nil
	}}
	d.Confirm(context.Background(), "Revoke alice?", "")
	if !strings.HasPrefix(script, `display dialog "Revoke alice?"`) {
		t.Fatalf("Confirm script %q should show the title as the body", script)
	}
	d.Choose(context.Background(), "Pick one", "", []string{"a"})
	if !strings.Contains(script, `with prompt "Pick one"`) {
		t.Fatalf("Choose script %q should show the title as the prompt", script)
	}
}

func TestDialogChooseGivesUpAfterFiveMinutes(t *testing.T) {
	var deadline time.Time
	d := Dialog{Run: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return []byte("a\n"), nil
	}}
	start := time.Now()
	d.Choose(context.Background(), "t", "d", []string{"a"})
	if got := deadline.Sub(start); got < 4*time.Minute || got > 6*time.Minute {
		t.Fatalf("Run's deadline is %v out, want about 5m", got)
	}
}

func TestDialogChooseDeadlineIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	d := Dialog{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("signal: killed")
	}}
	if got, err := d.Choose(ctx, "t", "d", []string{"a"}); got != "" || err != nil {
		t.Fatalf("Choose = %q, %v; want \"\", nil", got, err)
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
	if _, ok := Default("linux", env(nil), lookPathFinds(), nil, nil).(NoHuman); !ok {
		t.Fatal("expected NoHuman when in/out are not terminals")
	}
}
