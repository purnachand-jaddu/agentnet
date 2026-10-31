package consent

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeDialog records one LinuxDialog run and answers with canned output.
type fakeDialog struct {
	stdout, stderr string
	err            error
	name           string
	args           []string
	deadline       time.Time
}

func (f *fakeDialog) run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.name, f.args = name, args
	f.deadline, _ = ctx.Deadline()
	return []byte(f.stdout), []byte(f.stderr), f.err
}

func linuxDialog(f *fakeDialog) LinuxDialog { return LinuxDialog{Tool: "/usr/bin/zenity", Run: f.run} }

func TestEscapeDialogText(t *testing.T) {
	for in, want := range map[string]string{
		`R&D <x> \n`: `R&amp;D &lt;x&gt; \\n`,
		"a\nb":       "a&#xA;b",
		"plain":      "plain",
	} {
		if got := escapeDialogText(in); got != want {
			t.Errorf("escapeDialogText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinuxDialogConfirmArgs(t *testing.T) {
	f := &fakeDialog{stdout: "Approve\n"}
	ok, err := linuxDialog(f).Confirm(context.Background(), "Add agentnet?", `/home/u/R&D`)
	if err != nil || !ok {
		t.Fatalf("Confirm = %v, %v", ok, err)
	}
	want := []string{"--list", "--hide-header", "--title=Add agentnet?", "--text=/home/u/R&amp;D", "--column=", "Deny", "Approve"}
	if f.name != "/usr/bin/zenity" || !slices.Equal(f.args, want) {
		t.Fatalf("ran %s %q, want %q", f.name, f.args, want)
	}
}

func TestLinuxDialogConfirmResults(t *testing.T) {
	for _, stdout := range []string{"Deny\n", "", "Approve\n2", "approve\n", "Approve|Deny\n"} {
		ok, err := linuxDialog(&fakeDialog{stdout: stdout}).Confirm(context.Background(), "t", "d")
		if ok || err != nil {
			t.Errorf("stdout %q → %v, %v; want false, nil", stdout, ok, err)
		}
	}
}

func TestLinuxDialogIgnoresStderrOnSuccess(t *testing.T) {
	f := &fakeDialog{stdout: "Approve\n", stderr: "Gtk-Message: GtkDialog mapped without a transient parent.\n"}
	if ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d"); !ok || err != nil {
		t.Fatalf("Confirm = %v, %v; want true, nil", ok, err)
	}
}

func TestLinuxDialogCancelIsNotAnError(t *testing.T) {
	f := &fakeDialog{err: fakeExitError{code: 1}}
	if ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d"); ok || err != nil {
		t.Fatalf("Confirm = %v, %v; want false, nil", ok, err)
	}
	if got, err := linuxDialog(f).Choose(context.Background(), "t", "d", []string{"a"}); got != "" || err != nil {
		t.Fatalf("Choose = %q, %v; want \"\", nil", got, err)
	}
}

func TestLinuxDialogExit1WithGtkNoiseIsCancel(t *testing.T) {
	f := &fakeDialog{stderr: "Gtk-Message: 12:00:00.000: GtkDialog mapped without a transient parent. This is discouraged.\n", err: fakeExitError{code: 1}}
	if ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d"); ok || err != nil {
		t.Fatalf("Confirm = %v, %v; want false, nil", ok, err)
	}
	if got, err := linuxDialog(f).Choose(context.Background(), "t", "d", []string{"a"}); got != "" || err != nil {
		t.Fatalf("Choose = %q, %v; want \"\", nil", got, err)
	}
}

func TestLinuxDialogNoDisplayIsNoHuman(t *testing.T) {
	for _, stderr := range []string{
		"(zenity:1234): Gtk-WARNING **: 10:00:00.000: cannot open display: :0\n",
		"qarma: could not connect to display :0\n",
		"Gtk-WARNING: Failed to open display\n",
	} {
		f := &fakeDialog{stdout: "Approve\n", stderr: stderr, err: fakeExitError{code: 1}}
		if ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d"); ok || !errors.Is(err, ErrNoHuman) {
			t.Errorf("stderr %q: Confirm = %v, %v; want false, ErrNoHuman", stderr, ok, err)
		}
		if _, err := linuxDialog(f).Choose(context.Background(), "t", "d", []string{"a"}); !errors.Is(err, ErrNoHuman) {
			t.Errorf("stderr %q: Choose err = %v; want ErrNoHuman", stderr, err)
		}
	}
}

func TestLinuxDialogOtherFailureCarriesStderr(t *testing.T) {
	for _, f := range []*fakeDialog{
		{stdout: "Approve\n", stderr: "zenity: unknown option --hide-header\n", err: fakeExitError{code: 255}},
		{stdout: "Approve\n", stderr: "terminated\n", err: fakeExitError{code: -1}},
	} {
		ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d")
		if ok || err == nil || !strings.Contains(err.Error(), strings.TrimSpace(f.stderr)) {
			t.Errorf("Confirm = %v, %v; want false and an error carrying %q", ok, err, f.stderr)
		}
	}
	f := &fakeDialog{err: exec.ErrNotFound}
	if ok, err := linuxDialog(f).Confirm(context.Background(), "t", "d"); ok || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Confirm = %v, %v; want false, ErrNotFound", ok, err)
	}
}

func TestLinuxDialogConfirmGivesUpAfterFiveMinutes(t *testing.T) {
	f := &fakeDialog{stdout: "Deny\n"}
	start := time.Now()
	linuxDialog(f).Confirm(context.Background(), "t", "d")
	if d := f.deadline.Sub(start); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("Run's deadline is %v out, want about 5m", d)
	}
}

func TestLinuxDialogDeadlineDenies(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	f := &fakeDialog{stdout: "Approve\n", err: errors.New("signal: killed")}
	if ok, err := linuxDialog(f).Confirm(ctx, "t", "d"); ok || err != nil {
		t.Fatalf("Confirm = %v, %v; want false, nil", ok, err)
	}
	if got, err := linuxDialog(f).Choose(ctx, "t", "d", []string{"Approve"}); got != "" || err != nil {
		t.Fatalf("Choose = %q, %v; want \"\", nil", got, err)
	}
}

func TestLinuxDialogChoose(t *testing.T) {
	f := &fakeDialog{stdout: "Join a network\n"}
	got, err := linuxDialog(f).Choose(context.Background(), "Set up", "Create or join?", []string{"Create a new network", "Join a network"})
	if err != nil || got != "Join a network" {
		t.Fatalf("Choose = %q, %v", got, err)
	}
	want := []string{"--list", "--hide-header", "--title=Set up", "--text=Create or join?", "--column=", "Create a new network", "Join a network"}
	if !slices.Equal(f.args, want) {
		t.Fatalf("args %q, want %q", f.args, want)
	}
	for _, stdout := range []string{"", "Join\n", "Join a network\nCreate a new network\n"} {
		got, err := linuxDialog(&fakeDialog{stdout: stdout}).Choose(context.Background(), "t", "d", []string{"Create a new network", "Join a network"})
		if got != "" || err != nil {
			t.Errorf("stdout %q → %q, %v; want \"\", nil", stdout, got, err)
		}
	}
}

func TestLinuxDialogChooseRejectsFlagLikeOptions(t *testing.T) {
	f := &fakeDialog{stdout: "--text=x\n"}
	got, err := linuxDialog(f).Choose(context.Background(), "t", "d", []string{"ok", "--text=x"})
	if got != "" || err == nil {
		t.Fatalf("Choose = %q, %v; want an error", got, err)
	}
	if f.args != nil {
		t.Fatalf("ran the dialog with %q", f.args)
	}
}

func TestLinuxTool(t *testing.T) {
	for _, c := range []struct {
		path []string
		want string
	}{
		{[]string{"zenity", "qarma"}, "/usr/bin/zenity"},
		{[]string{"qarma", "matedialog"}, "/usr/bin/qarma"},
		{[]string{"matedialog"}, "/usr/bin/matedialog"},
		{nil, ""},
	} {
		if got := linuxTool(lookPathFinds(c.path...)); got != c.want {
			t.Errorf("PATH %v → %q, want %q", c.path, got, c.want)
		}
	}
}

func TestLinuxDialogEmptyDetailShowsTitle(t *testing.T) {
	f := &fakeDialog{}
	linuxDialog(f).Confirm(context.Background(), "Revoke R&D?", "")
	if !slices.Contains(f.args, "--text=Revoke R&amp;D?") {
		t.Fatalf("args %q should show the escaped title as the body", f.args)
	}
	linuxDialog(f).Choose(context.Background(), "Pick one", "", []string{"a"})
	if !slices.Contains(f.args, "--text=Pick one") {
		t.Fatalf("args %q should show the title as the body", f.args)
	}
}
