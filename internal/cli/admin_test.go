package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/consent"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

// startFakeDaemon serves h on paths.Socket() until t ends, so a CLI test can exercise the
// daemon-is-running path without a real daemon.Daemon.
func startFakeDaemon(t *testing.T, h ipc.Handler) {
	t.Helper()
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := ipc.Listen(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ipc.Serve(ctx, l, h, nil) }()
	t.Cleanup(func() { cancel(); <-done })
}

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

// TestRevokeToleratesDaemonNotRunning proves a missing daemon is not a reload failure: revoke still
// succeeds, since no running daemon means no stale state to leave behind.
func TestRevokeToleratesDaemonNotRunning(t *testing.T) {
	_, _, _, _ = bundleAndCodes(t)
	out, errOut := withIO(t)
	if code := Main([]string{"revoke", "sam"}, "v0"); code != 0 {
		t.Fatalf("code = %d, want 0; stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "revoked sam@xps") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestRevokeReportsDaemonReloadFailure proves a running daemon that fails "team_changed" is reported
// distinctly from success: the revocation is still saved locally, but the CLI exits 1 and says so.
func TestRevokeReportsDaemonReloadFailure(t *testing.T) {
	_, _, _, _ = bundleAndCodes(t)

	startFakeDaemon(t, func(ctx context.Context, c ipc.Conn, op string, args json.RawMessage) (any, error) {
		switch op {
		case "status":
			return daemon.StatusResult{PID: 123, Service: false}, nil
		case "team_changed":
			return nil, errors.New("boom")
		default:
			return nil, errors.New("unexpected op " + op)
		}
	})

	tm := loadTeam(t)
	member := tm.ByPerson("sam")[0]

	_, errOut := withIO(t)
	if code := Main([]string{"revoke", "sam"}, "v0"); code != 1 {
		t.Fatalf("code = %d, want 1; stderr %q", code, errOut.String())
	}
	want := "revocation saved, but the running daemon didn't reload: boom; restart it with `agentnet daemon`"
	if !strings.Contains(errOut.String(), want) {
		t.Fatalf("stderr = %q, want to contain %q", errOut.String(), want)
	}

	// The local revoke itself must still have taken effect, even though live activation failed.
	after := loadTeam(t)
	if err := after.Verify(member, time.Now()); err == nil {
		t.Fatal("the block should still be persisted locally despite the daemon's reload failure")
	}
}

// TestRevokeReportsServiceReloadFailure checks the hint names `agentnet setup` when the answering
// daemon is the installed service, not a bare one.
func TestRevokeReportsServiceReloadFailure(t *testing.T) {
	_, _, _, _ = bundleAndCodes(t)

	startFakeDaemon(t, func(ctx context.Context, c ipc.Conn, op string, args json.RawMessage) (any, error) {
		switch op {
		case "status":
			return daemon.StatusResult{PID: 123, Service: true}, nil
		case "team_changed":
			return nil, errors.New("boom")
		default:
			return nil, errors.New("unexpected op " + op)
		}
	})

	_, errOut := withIO(t)
	if code := Main([]string{"revoke", "sam"}, "v0"); code != 1 {
		t.Fatalf("code = %d, want 1; stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "restart it with `agentnet setup`") {
		t.Fatalf("stderr = %q, want the service restart hint", errOut.String())
	}
}
