package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/consent"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

var pairingCodeRE = regexp.MustCompile(`[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}`)
var strictPairingCodeRE = regexp.MustCompile(`^[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}$`)

// fakeApprover approves every Confirm and picks the first option on Choose.
type fakeApprover struct{}

func (fakeApprover) Confirm(ctx context.Context, title, detail string) (bool, error) {
	return true, nil
}
func (fakeApprover) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	if len(options) == 0 {
		return "", nil
	}
	return options[0], nil
}

// denyApprover denies every Confirm and cancels every Choose.
type denyApprover struct{}

func (denyApprover) Confirm(ctx context.Context, title, detail string) (bool, error) {
	return false, nil
}
func (denyApprover) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	return "", nil
}

// withApprover injects a into newApprover for the duration of t, restoring the original after.
func withApprover(t *testing.T, a consent.Approver) {
	t.Helper()
	old := newApprover
	newApprover = func() consent.Approver { return a }
	t.Cleanup(func() { newApprover = old })
}

func TestJoinNoHumanLeavesNoMachineKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	_, errOut := withIO(t)
	withApprover(t, consent.NoHuman{})

	if code := Main([]string{"join", "--person", "sam", "--machine", "xps"}, "v0"); code == 0 {
		t.Fatalf("code = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "needs a person to approve") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, "machine.key")); !os.IsNotExist(err) {
		t.Fatalf("machine.key exists or stat failed: %v", err)
	}
}

// bundleAndCodes runs init on a fresh founder home and join+admit on a fresh joiner home, returning the
// bundle `accept` would take, the team code `init` printed, the pairing code `join` printed, and the
// joiner's AGENTNET_HOME (accept must run there: that's where join generated the machine key the bundle names).
func bundleAndCodes(t *testing.T) (bundle, teamCode, pairingCode, joinerHome string) {
	t.Helper()
	founderHome := t.TempDir()
	joinerHome = t.TempDir()
	withApprover(t, fakeApprover{})

	t.Setenv("AGENTNET_HOME", founderHome)
	out, _ := withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatalf("init: code %d, stdout %q", code, out.String())
	}
	teamCode = pairingCodeRE.FindString(out.String())

	t.Setenv("AGENTNET_HOME", joinerHome)
	out, _ = withIO(t)
	if code := Main([]string{"join", "--person", "sam", "--machine", "xps"}, "v0"); code != 0 {
		t.Fatalf("join: code %d, stdout %q", code, out.String())
	}
	joinOut := strings.TrimSpace(out.String())
	reqLine := strings.Split(joinOut, "\n")[0]
	pairingCode = pairingCodeRE.FindString(joinOut)

	t.Setenv("AGENTNET_HOME", founderHome)
	out, _ = withIO(t)
	if code := Main([]string{"admit", reqLine}, "v0"); code != 0 {
		t.Fatalf("admit: code %d, stdout %q", code, out.String())
	}
	bundle = strings.Split(strings.TrimSpace(out.String()), "\n")[0]
	return bundle, teamCode, pairingCode, joinerHome
}

func TestAcceptDenialLeavesNoTeam(t *testing.T) {
	bundle, _, _, _ := bundleAndCodes(t)

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withIO(t)
	withApprover(t, denyApprover{})

	if code := Main([]string{"accept", bundle}, "v0"); code == 0 {
		t.Fatalf("code = 0, want non-zero")
	}
	if _, err := os.Stat(filepath.Join(home, "agentnet.db")); !os.IsNotExist(err) {
		t.Fatalf("agentnet.db exists or stat failed: %v", err)
	}
}

// A config-save failure after team.Create has already succeeded must not strand the network: the founder
// key must survive, init must still report success (the person/machine already live in the self cert),
// and a later init must see the existing network rather than trying to create a second one.
func TestInitConfigSaveFailureKeepsFounderKeyAndWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})

	old := saveConfigPersonMachine
	saveConfigPersonMachine = func(person, machine string) error { return errors.New("forced config save failure") }
	t.Cleanup(func() { saveConfigPersonMachine = old })

	out, errOut := withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatalf("init should exit 0 even when config save fails; stdout %q, stderr %q", out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "warning") {
		t.Fatalf("stderr = %q, want a warning", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, "founder.key")); err != nil {
		t.Fatalf("founder.key should still exist: %v", err)
	}

	saveConfigPersonMachine = old // restore before the second init, which only needs to see the existing team
	out2, errOut2 := withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "other"}, "v0"); code == 0 {
		t.Fatalf("second init should refuse: already in network; stdout %q", out2.String())
	}
	if !strings.Contains(errOut2.String(), "already in network") {
		t.Fatalf("stderr = %q, want \"already in network\"", errOut2.String())
	}
}

// init must never replace an existing network, even if the founder key was moved offline after init.
func TestInitRefusesWhenAlreadyInNetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatal("first init failed")
	}

	out, errOut := withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "other"}, "v0"); code == 0 {
		t.Fatalf("second init should fail, stdout %q", out.String())
	}
	if !strings.Contains(errOut.String(), "already in network") {
		t.Fatalf("stderr = %q, want \"already in network\"", errOut.String())
	}
}

// A failure after the founder key is written (here: machine.key is a directory, so generating the machine
// key fails) must not leave the founder key behind, or a rerun would wrongly refuse with "already exists".
func TestInitRemovesFounderKeyOnLaterFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	withIO(t)

	if err := os.MkdirAll(filepath.Join(home, "machine.key"), 0o700); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code == 0 {
		t.Fatal("init should have failed")
	}
	if _, err := os.Stat(filepath.Join(home, "founder.key")); !os.IsNotExist(err) {
		t.Fatalf("founder.key should have been removed after the failure, stat err = %v", err)
	}
}

func TestInitRejectsBadPersonOrMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	_, errOut := withIO(t)

	if code := Main([]string{"init", "--person", "Alex", "--machine", "mbp"}, "v0"); code == 0 {
		t.Fatal("init with a bad person name should fail")
	}
	if !strings.Contains(errOut.String(), "lowercase") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, "founder.key")); !os.IsNotExist(err) {
		t.Fatal("founder.key should not have been written")
	}
}

func TestJoinRejectsBadPersonOrMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	_, errOut := withIO(t)

	if code := Main([]string{"join", "--person", "sam", "--machine", "my mac"}, "v0"); code == 0 {
		t.Fatal("join with a bad machine name should fail")
	}
	if !strings.Contains(errOut.String(), "lowercase") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, "machine.key")); !os.IsNotExist(err) {
		t.Fatal("machine.key should not have been written")
	}
}

// accept must require a prior `join` on this machine, never generate a fresh machine key of its own.
func TestAcceptRequiresExistingMachineKey(t *testing.T) {
	bundle, _, _, _ := bundleAndCodes(t)

	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	_, errOut := withIO(t)

	if code := Main([]string{"accept", bundle}, "v0"); code == 0 {
		t.Fatal("accept without a prior join should fail")
	}
	if !strings.Contains(errOut.String(), "agentnet join") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, "machine.key")); !os.IsNotExist(err) {
		t.Fatal("accept must not generate a machine key of its own")
	}
}

func TestInitJoinAdmitAcceptFullFlow(t *testing.T) {
	founderHome := t.TempDir()
	joinerHome := t.TempDir()
	withApprover(t, fakeApprover{})

	t.Setenv("AGENTNET_HOME", founderHome)
	out, _ := withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatalf("init: code %d, stdout %q", code, out.String())
	}
	teamCode := pairingCodeRE.FindString(out.String())
	if teamCode == "" {
		t.Fatalf("init printed no team code: %q", out.String())
	}

	t.Setenv("AGENTNET_HOME", joinerHome)
	out, _ = withIO(t)
	if code := Main([]string{"join", "--person", "sam", "--machine", "xps"}, "v0"); code != 0 {
		t.Fatalf("join: code %d, stdout %q", code, out.String())
	}
	joinOut := strings.TrimSpace(out.String())
	reqLine := strings.Split(joinOut, "\n")[0]
	pairingCode := pairingCodeRE.FindString(joinOut)
	if pairingCode == "" {
		t.Fatalf("join printed no pairing code: %q", joinOut)
	}

	t.Setenv("AGENTNET_HOME", founderHome)
	out, _ = withIO(t)
	if code := Main([]string{"admit", reqLine}, "v0"); code != 0 {
		t.Fatalf("admit: code %d, stdout %q", code, out.String())
	}
	admitOut := strings.TrimSpace(out.String())
	bundleLine := strings.Split(admitOut, "\n")[0]
	if got := pairingCodeRE.FindString(admitOut); got != teamCode {
		t.Fatalf("admit printed team code %q, want %q", got, teamCode)
	}

	t.Setenv("AGENTNET_HOME", joinerHome)
	out, _ = withIO(t)
	if code := Main([]string{"accept", bundleLine}, "v0"); code != 0 {
		t.Fatalf("accept: code %d, stdout %q", code, out.String())
	}

	founderStore, err := sqlite.Open(filepath.Join(founderHome, "agentnet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer founderStore.Close()
	founderState, err := team.Load(founderStore)
	if err != nil || founderState == nil {
		t.Fatalf("founder team.Load: %v, %v", founderState, err)
	}

	joinerStore, err := sqlite.Open(filepath.Join(joinerHome, "agentnet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer joinerStore.Close()
	joinerState, err := team.Load(joinerStore)
	if err != nil || joinerState == nil {
		t.Fatalf("joiner team.Load: %v, %v", joinerState, err)
	}

	if err := joinerState.Verify(founderState.Self, time.Now()); err != nil {
		t.Fatalf("joiner can't verify founder's self cert: %v", err)
	}
	joinerCert, ok := founderState.Members[base64.RawURLEncoding.EncodeToString(joinerState.Self.Key)]
	if !ok {
		t.Fatal("founder has no record of the joiner")
	}
	if err := founderState.Verify(joinerCert, time.Now()); err != nil {
		t.Fatalf("founder can't verify joiner's cert: %v", err)
	}

	for _, code := range []string{teamCode, pairingCode} {
		if !strictPairingCodeRE.MatchString(code) {
			t.Fatalf("code %q doesn't match XXXX-XXXX-XXXX", code)
		}
	}
}
