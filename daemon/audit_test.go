package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/message"
)

// newAuditor opens a FileAuditor at a fresh path in t's temp dir, closing it at cleanup.
func newAuditor(t *testing.T) (*FileAuditor, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	au, err := NewFileAuditor(path, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { au.Close() })
	return au, path
}

// readAudit parses path's JSON lines into AuditEntry values.
func readAudit(t *testing.T, path string) []AuditEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditEntry
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuditFileMode0600(t *testing.T) {
	_, path := newAuditor(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestAuditRecordNeverCarriesBody(t *testing.T) {
	au, path := newAuditor(t)
	body := "the secret body nobody should see"
	au.Record(AuditEntry{TS: 1, Dir: "in", ID: "m1", From: "sam/terminal", To: "alex/a", State: message.StateHeld, BodySHA: bodyHash(body)})

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.BodySHA != bodyHash(body) || e.BodySHA == body {
		t.Fatalf("BodySHA = %q", e.BodySHA)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), body) {
		t.Fatal("audit file contains the body text")
	}
}

func TestAuditInboundRecordsOneLinePerDecision(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	b := newB(t, f, 0)
	b.d.opts.Auditor = au // PolicyFor stays nil: default accept, matching other receive_test.go scenarios

	m := newMsg("alex/terminal", "sam/web-codex")
	ack := fromA(t, f, b, m)
	if ack.State != message.StateQueued {
		t.Fatalf("ack %+v, want queued", ack)
	}

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.Dir != "in" || e.ID != m.ID || e.From != m.From || e.To != m.To || e.State != message.StateQueued {
		t.Fatalf("entry %+v", e)
	}
	if e.BodySHA != bodyHash(m.Body) {
		t.Fatalf("BodySHA = %q, want hash of body", e.BodySHA)
	}

	// a second, held decision appends its own extra line, not a duplicate of the first.
	withPolicy(b.d, config.Hold)
	held := newMsg("alex/terminal", "sam/web-codex")
	ack = fromA(t, f, b, held)
	if ack.State != message.StateHeld {
		t.Fatalf("ack %+v, want held", ack)
	}
	entries = readAudit(t, path)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}
	if entries[1].ID != held.ID || entries[1].State != message.StateHeld {
		t.Fatalf("entries[1] = %+v", entries[1])
	}
}

func TestAuditOutboundRecordsEachAttempt(t *testing.T) {
	f := newTeamFixture(t)
	au, path := newAuditor(t)
	aPort, bPort := freePort(t), freePort(t)
	a := newNetDaemon(t, "alex", "mbp", f.aStore, f.aTeam, f.aKeys, aPort, lanAddr(bPort))
	a.opts.Auditor = au
	b := startB(t, f, bPort, aPort)
	runDaemon(t, a)
	waitUntil(t, 5*time.Second, "A lists sam/web-codex", func() bool { return statusOf(a, "sam/web-codex") == "idle" })

	res := sendOne(t, a, "cli:terminal", "sam/web-codex")
	id := res.IDs[0]
	waitUntil(t, 2*time.Second, "B gets it", func() bool { return len(b.fake.Pushed()) == 1 })

	entries := readAudit(t, path)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	e := entries[0]
	if e.Dir != "out" || e.ID != id || e.From != "alex/terminal" || e.To != "sam/web-codex" {
		t.Fatalf("entry %+v", e)
	}
	if e.BodySHA == "" || strings.Contains(e.BodySHA, "hello") {
		t.Fatalf("BodySHA = %q", e.BodySHA)
	}
}
