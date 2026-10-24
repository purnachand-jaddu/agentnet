package cli

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/consent"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

// loadTeam opens the store at the current AGENTNET_HOME and loads its team state.
func loadTeam(t *testing.T) *team.State {
	t.Helper()
	st, err := sqlite.Open(paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tm, err := team.Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if tm == nil {
		t.Fatal("no team on this machine")
	}
	return tm
}

// TestRevokeNoHumanExitsNonZeroNoChange proves revoke needs a person to approve, and leaves team state untouched.
func TestRevokeNoHumanExitsNonZeroNoChange(t *testing.T) {
	_, _, _, _ = bundleAndCodes(t) // leaves AGENTNET_HOME on the founder's home, sam@xps admitted

	tmBefore := loadTeam(t)
	member := tmBefore.ByPerson("sam")[0]

	withApprover(t, consent.NoHuman{})
	_, errOut := withIO(t)
	if code := Main([]string{"revoke", "sam"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "needs a person to approve") {
		t.Fatalf("stderr = %q", errOut.String())
	}

	tmAfter := loadTeam(t)
	if err := tmAfter.Verify(member, time.Now()); err != nil {
		t.Fatalf("member should still be unblocked: %v", err)
	}
}

// TestRevokeRefusesFounderOwnMachine proves revoke refuses to block the founder's own device cert.
func TestRevokeRefusesFounderOwnMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTNET_HOME", home)
	withApprover(t, fakeApprover{})
	withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatal("init failed")
	}

	_, errOut := withIO(t)
	if code := Main([]string{"revoke", "alex"}, "v0"); code == 0 {
		t.Fatal("code = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "you can't revoke the founder's own machine") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestRevokeByPersonBlocksAllKeys admits the same person on two machines, then revokes by person and
// checks both certs are blocked.
func TestRevokeByPersonBlocksAllKeys(t *testing.T) {
	founderHome := t.TempDir()
	withApprover(t, fakeApprover{})

	t.Setenv("AGENTNET_HOME", founderHome)
	withIO(t)
	if code := Main([]string{"init", "--person", "alex", "--machine", "mbp"}, "v0"); code != 0 {
		t.Fatal("init failed")
	}

	admitAs := func(machine string) {
		t.Helper()
		joinerHome := t.TempDir()
		t.Setenv("AGENTNET_HOME", joinerHome)
		out, _ := withIO(t)
		if code := Main([]string{"join", "--person", "sam", "--machine", machine}, "v0"); code != 0 {
			t.Fatalf("join %s: code %d, stdout %q", machine, code, out.String())
		}
		reqLine := strings.Split(strings.TrimSpace(out.String()), "\n")[0]

		t.Setenv("AGENTNET_HOME", founderHome)
		out, _ = withIO(t)
		if code := Main([]string{"admit", reqLine}, "v0"); code != 0 {
			t.Fatalf("admit %s: code %d, stdout %q", machine, code, out.String())
		}
	}
	admitAs("xps")
	admitAs("imac") // same person, second machine; fakeApprover approves the "same person?" follow-up

	tm := loadTeam(t)
	certs := tm.ByPerson("sam")
	if len(certs) != 2 {
		t.Fatalf("sam should have 2 certs before revoke, got %d", len(certs))
	}

	withIO(t)
	if code := Main([]string{"revoke", "sam"}, "v0"); code != 0 {
		t.Fatal("revoke by person should succeed")
	}

	after := loadTeam(t)
	for _, c := range certs {
		if err := after.Verify(c, time.Now()); err == nil {
			t.Fatalf("cert for sam@%s should be blocked after revoke by person", c.Machine)
		}
	}
}

// TestRevokeByKeyExactMatch revokes a single member by their base64url key rather than their person name.
func TestRevokeByKeyExactMatch(t *testing.T) {
	_, _, _, _ = bundleAndCodes(t)
	tm := loadTeam(t)
	member := tm.ByPerson("sam")[0]
	key := base64.RawURLEncoding.EncodeToString(member.Key)

	withIO(t)
	if code := Main([]string{"revoke", key}, "v0"); code != 0 {
		t.Fatal("revoke by key should succeed")
	}

	after := loadTeam(t)
	if err := after.Verify(member, time.Now()); err == nil {
		t.Fatal("member should be blocked after revoke by key")
	}
}
