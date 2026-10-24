package cli

import (
	"context"
	"encoding/base64"
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
