package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

func TestNamesAndCollisions(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	a := register(t, d, &fakeConn{id: 1}, "codex", "1", "/src/api", false)
	b := register(t, d, &fakeConn{id: 2}, "codex", "2", "/src/api", false)
	if a.Address != "alex/api-codex" || b.Address != "alex/api-codex-2" {
		t.Fatalf("got %q, %q", a.Address, b.Address)
	}
}

// TestListenNameIsHonoredAndKept checks that a `listen --as terminal` registration is named "terminal"
// (not a cwd-derived name), that it's reachable as alex/terminal, that its reply round-trips back to
// the sender, and that its remembered record is Kept (never forgotten).
func TestListenNameIsHonoredAndKept(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))

	reg := call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{
		Session: adapter.Session{Key: "cli:terminal", Agent: "cli", ID: "terminal", Cwd: "/src/api"},
		Name:    "terminal",
	})
	if reg.Name != "terminal" {
		t.Fatalf("Name = %q, want terminal", reg.Name)
	}

	agentReg := register(t, d, &fakeConn{id: 2}, "codex", "1", "/src/api", false)

	sendToTerminal := call[SendResult](t, d, &fakeConn{id: 2}, "send",
		SendArgs{From: agentReg.Key, To: "alex/terminal", Body: "hi", Wake: true})
	if len(sendToTerminal.IDs) != 1 {
		t.Fatalf("send to alex/terminal: %+v", sendToTerminal)
	}

	inboxTerminal := call[InboxResult](t, d, &fakeConn{id: 1}, "inbox", InboxArgs{Key: "cli:terminal", Peek: true})
	if inboxTerminal.Text == "" {
		t.Fatal("cli:terminal has no mail")
	}

	replyRes := call[SendResult](t, d, &fakeConn{id: 1}, "send",
		SendArgs{From: "cli:terminal", To: "alex/" + agentReg.Name, Body: "hey back", Wake: true})
	if len(replyRes.IDs) != 1 {
		t.Fatalf("cli reply send: %+v", replyRes)
	}
	inboxAgent := call[InboxResult](t, d, &fakeConn{id: 2}, "inbox", InboxArgs{Key: agentReg.Key, Peek: true})
	if inboxAgent.Text == "" {
		t.Fatal("agent session did not receive the cli's reply")
	}

	rec, err := d.opts.Store.SessionByKey("cli:terminal")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || !rec.Kept {
		t.Fatalf("remembered record Kept = %+v, want true", rec)
	}
}

func TestReturningSessionGetsItsNameBack(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	c := &fakeConn{id: 1}
	register(t, d, c, "codex", "1", "/src/api", false)
	d.ConnClosed(c)
	register(t, d, &fakeConn{id: 2}, "codex", "2", "/src/api", false) // takes the next free name
	back := register(t, d, &fakeConn{id: 3}, "codex", "1", "/src/api", false)
	if back.Name != "api-codex" {
		t.Fatalf("returning session named %q", back.Name)
	}
}

// TestHookRegisterClearsInboxFromOtherSessionsOfSameAgent checks that only the most recent key
// claiming an inbox socket keeps it: a stale pre-/clear hook can't re-arm direct posts into it.
func TestHookRegisterClearsInboxFromOtherSessionsOfSameAgent(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelHook))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:x1", Agent: "codex", ID: "x1", Cwd: "/src/web", Inbox: "/tmp/sock"}, Hook: true})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:x2", Agent: "codex", ID: "x2", Cwd: "/src/web2", Inbox: "/tmp/sock"}, Hook: true})

	d.mu.Lock()
	inbox1 := d.live["codex:x1"].s.Inbox
	inbox2 := d.live["codex:x2"].s.Inbox
	d.mu.Unlock()
	if inbox1 != "" || inbox2 != "/tmp/sock" {
		t.Fatalf("expected only the latest key to hold the inbox, got x1=%q x2=%q", inbox1, inbox2)
	}
}

func TestMCPAttachesToHookSessionByPID(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	r := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	if r.Key != "claude-code:c1" || r.Name != "api-claude" {
		t.Fatalf("MCP made a second session: %+v", r)
	}
}

func TestOffFolderRefused(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	d.opts.FolderAllowed = func(string) bool { return false }
	raw, _ := json.Marshal(RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/private"}})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 1}, "register", raw); err == nil {
		t.Fatal("registered in a folder that is off")
	}
}

// failingNameStore fails every SessionByName call, to prove register surfaces the error instead of spinning forever.
type failingNameStore struct {
	store.Store
	err error
}

func (f *failingNameStore) SessionByName(string) (*store.SessionRecord, error) { return nil, f.err }

func TestRegisterPropagatesStoreError(t *testing.T) {
	st := &failingNameStore{Store: storetest.SQLite(t), err: errors.New("boom")}
	d := New(Options{Person: "alex", Machine: "mbp", Store: st, Adapters: adapter.NewRegistry(codexFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf})
	raw, _ := json.Marshal(RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api"}})

	done := make(chan error, 1)
	go func() {
		_, err := d.Handle(context.Background(), &fakeConn{id: 1}, "register", raw)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("register succeeded despite a failing store")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("register hung with a failing store (mutex deadlock)")
	}
}

// steppingClock returns a Now func that advances by one second on every call, for deterministic lastHook ordering in tests.
func steppingClock() func() time.Time {
	now := time.Unix(1700000000, 0)
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

func TestMCPAttachPicksMostRecentHookSession(t *testing.T) {
	d := New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(claudeFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: steppingClock(), Log: t.Logf})
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c2", Agent: "claude-code", ID: "c2", Cwd: "/src/api", PID: 4242}, Hook: true})

	r := call[Registered](t, d, &fakeConn{id: 3}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	if r.Key != "claude-code:c2" {
		t.Fatalf("MCP attached to %q, want the more recently hook-registered claude-code:c2", r.Key)
	}
}

func TestMCPFallsBackOnAmbiguousHookTie(t *testing.T) {
	fixed := func() time.Time { return time.Unix(1700000000, 0) } // every call gets the same timestamp: a guaranteed tie
	d := New(Options{Person: "alex", Machine: "mbp", Store: storetest.SQLite(t), Adapters: adapter.NewRegistry(claudeFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: fixed, Log: t.Logf})
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c2", Agent: "claude-code", ID: "c2", Cwd: "/src/api", PID: 4242}, Hook: true})

	r := call[Registered](t, d, &fakeConn{id: 3}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	if r.Key != "proc:999" {
		t.Fatalf("expected the MCP to register under its own key on an ambiguous tie, got %q", r.Key)
	}
}

func TestMCPRegistrationUnderOwnKeyIsRemembered(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	rec, err := d.opts.Store.SessionByKey("claude-code:c1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("claude-code:c1 was not remembered; its name could be taken by another session after a restart")
	}
}

func TestMCPPlaceholderIsNotRemembered(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	rec, err := d.opts.Store.SessionByKey("proc:999")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("proc:999 placeholder was remembered")
	}
}

func TestPlainRegisterIsNotHookSeen(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	c := &fakeConn{id: 1}
	r := register(t, d, c, "codex", "1", "/src/api", false) // no Hook, no MCPPID

	d.ConnClosed(c)

	d.mu.Lock()
	_, stillLive := d.live[r.Key]
	d.mu.Unlock()
	if stillLive {
		t.Fatal("a plain (non-hook) register survived ConnClosed; it should have no hook evidence")
	}
}

func TestRegisterMergeKeepsExistingFields(t *testing.T) {
	d := newTestDaemon(t, codexFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api", PID: 111, Inbox: "sock1"}})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "codex:1", Agent: "codex", ID: "1", Cwd: "/src/api"}})

	d.mu.Lock()
	s := d.live["codex:1"].s
	d.mu.Unlock()
	if s.PID != 111 || s.Inbox != "sock1" {
		t.Fatalf("bare re-register wiped fields: %+v", s)
	}
}

func TestReverseJoinMCPThenHook(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	r := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})

	if r.Key != "claude-code:c1" {
		t.Fatalf("got key %q, want claude-code:c1", r.Key)
	}
	if r.Name != "api-claude" {
		t.Fatalf("got name %q, want api-claude (the proc: placeholder should have freed it)", r.Name)
	}
	d.mu.Lock()
	l, stillLive := d.live["claude-code:c1"]
	_, stillProc := d.live["proc:999"]
	d.mu.Unlock()
	if !stillLive || !l.mcpPush {
		t.Fatalf("mcpPush not merged onto claude-code:c1: %+v", l)
	}
	if stillProc {
		t.Fatal("proc:999 placeholder was not removed after the reverse join")
	}
}

// TestHookReRegisterDoesNotClearMCPConn checks that once an MCP process has forward-joined a hook
// session, a later hook re-register leaves its conn/mcpPush alone, so closing the hook's own
// (never-recorded) connection can't drop the MCP link.
func TestHookReRegisterDoesNotClearMCPConn(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	hookConn := &fakeConn{id: 3}
	call[Registered](t, d, hookConn, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})

	d.ConnClosed(hookConn)

	d.mu.Lock()
	l, ok := d.live["claude-code:c1"]
	d.mu.Unlock()
	if !ok || !l.mcpPush || l.conn == nil {
		t.Fatalf("hook re-register's conn closing broke the MCP link: live=%v, %+v", ok, l)
	}
}

// failAfterNStore lets the first n SessionByName calls through, then fails every one after, so an
// early (setup) naming attempt succeeds and a later one fails deterministically.
type failAfterNStore struct {
	store.Store
	n     int
	calls int
}

func (f *failAfterNStore) SessionByName(name string) (*store.SessionRecord, error) {
	f.calls++
	if f.calls > f.n {
		return nil, errors.New("boom")
	}
	return f.Store.SessionByName(name)
}

// TestReverseJoinNamingFailureLeavesPlaceholderLive checks that when naming the incoming hook
// session fails, the reverse join never ran: the MCP placeholder is still live with its conn,
// not stranded without one.
func TestReverseJoinNamingFailureLeavesPlaceholderLive(t *testing.T) {
	st := &failAfterNStore{Store: storetest.SQLite(t), n: 1} // 1 call: the placeholder's own naming succeeds; the hook's fails
	d := New(Options{Person: "alex", Machine: "mbp", Store: st, Adapters: adapter.NewRegistry(claudeFake(adapter.ChannelPull)),
		FolderAllowed: func(string) bool { return true }, Now: time.Now, Log: t.Logf})

	procConn := &fakeConn{id: 1}
	call[Registered](t, d, procConn, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})

	raw, _ := json.Marshal(RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})
	if _, err := d.Handle(context.Background(), &fakeConn{id: 2}, "register", raw); err == nil {
		t.Fatal("expected the naming failure to surface as an error")
	}

	d.mu.Lock()
	proc, stillLive := d.live["proc:999"]
	d.mu.Unlock()
	if !stillLive || proc.conn != procConn {
		t.Fatalf("naming failure stranded the placeholder: live=%v, %+v", stillLive, proc)
	}
}

// TestRehomeWakesWaitingInboxOnNewKey checks that the reverse join's Rehome broadcasts arrival,
// so an "inbox" long poll already waiting on the new key wakes promptly instead of only finding
// the rehomed mail after its own wait_secs times out.
func TestRehomeWakesWaitingInboxOnNewKey(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:999", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 4242})
	call[SendResult](t, d, &fakeConn{id: 2}, "send", SendArgs{From: "cli:terminal", To: "alex/api-claude", Body: "hi", Wake: true})

	done := make(chan InboxResult, 1)
	go func() {
		done <- call[InboxResult](t, d, &fakeConn{id: 3}, "inbox", InboxArgs{Key: "claude-code:c1", WaitSecs: 5})
	}()
	time.Sleep(50 * time.Millisecond)

	call[Registered](t, d, &fakeConn{id: 4}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 4242}, Hook: true})

	select {
	case r := <-done:
		if !strings.Contains(r.Text, "hi") {
			t.Fatalf("got %q", r.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbox on the new key was not woken promptly after the rehome")
	}
}

// TestNameFreeOnlyExemptsTheFoldedPlaceholder checks that the placeholder-name exemption is scoped
// to the one placeholder a reverse join is actually folding, not every live placeholder: two
// unrelated id-less MCP registrations must get distinct names, and a keyed session that isn't
// folding a given placeholder must not take its name.
func TestNameFreeOnlyExemptsTheFoldedPlaceholder(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	a := call[Registered](t, d, &fakeConn{id: 1}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:111", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 1111})
	b := call[Registered](t, d, &fakeConn{id: 2}, "register", RegisterArgs{Session: adapter.Session{Key: "proc:222", Agent: "claude-code", Cwd: "/src/api"}, MCPPush: true, MCPPID: 2222})
	if a.Name == b.Name {
		t.Fatalf("two unrelated id-less MCP registrations got the same name %q", a.Name)
	}

	// A hook session with a PID that matches neither placeholder doesn't fold either one, so it
	// must not take proc:111's name.
	c := call[Registered](t, d, &fakeConn{id: 3}, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/api", PID: 9999}, Hook: true})
	if c.Name == a.Name {
		t.Fatalf("a keyed session took a live placeholder's name without folding it: %q", c.Name)
	}
}

// TestUnregisterDetachesCallersConnEvenWhenHookKeepsSessionLive checks that unregister always
// clears conn/mcpPush/Inbox/InboxToken for the caller's own connection, even when the session's
// hooks ran recently enough to keep it live: otherwise a stale MCP process would keep being
// pushed to (directly, or via ChannelCommand's leftover Inbox) for a session it no longer
// serves (e.g. after /clear moved it to a new session id).
func TestUnregisterDetachesCallersConnEvenWhenHookKeepsSessionLive(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	mcpConn := &fakeConn{id: 1}
	call[Registered](t, d, mcpConn, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:old", Agent: "claude-code", ID: "old", Cwd: "/src/c", Inbox: "sock", InboxToken: "tok"}, MCPPush: true})

	// The session's hooks just ran: lastHook is recent, well inside hookGrace.
	d.mu.Lock()
	d.live["claude-code:old"].hookSeen = true
	d.live["claude-code:old"].lastHook = d.opts.Now()
	d.mu.Unlock()

	call[any](t, d, mcpConn, "unregister", UnregisterArgs{Key: "claude-code:old"})

	d.mu.Lock()
	l, stillLive := d.live["claude-code:old"]
	d.mu.Unlock()
	if !stillLive {
		t.Fatal("hooks kept it live, but it's gone")
	}
	if l.conn != nil || l.mcpPush {
		t.Fatalf("unregister did not detach the caller's conn: conn=%v mcpPush=%v", l.conn, l.mcpPush)
	}
	if l.s.Inbox != "" || l.s.InboxToken != "" {
		t.Fatalf("unregister left Inbox/InboxToken behind: %+v", l.s)
	}
}

// TestUnregisterLeavesAnotherConnAlone checks that unregister only detaches the caller's own
// conn: a session served by a different connection than the one calling unregister is untouched.
func TestUnregisterLeavesAnotherConnAlone(t *testing.T) {
	d := newTestDaemon(t, claudeFake(adapter.ChannelPull))
	owner := &fakeConn{id: 1}
	call[Registered](t, d, owner, "register", RegisterArgs{Session: adapter.Session{Key: "claude-code:c1", Agent: "claude-code", ID: "c1", Cwd: "/src/c"}, MCPPush: true})

	// Keep it live across the unregister below, so a wrongly-detached conn wouldn't be masked
	// by the session simply being removed outright.
	d.mu.Lock()
	d.live["claude-code:c1"].hookSeen = true
	d.live["claude-code:c1"].lastHook = d.opts.Now()
	d.mu.Unlock()

	other := &fakeConn{id: 2}
	call[any](t, d, other, "unregister", UnregisterArgs{Key: "claude-code:c1"})

	d.mu.Lock()
	l, stillLive := d.live["claude-code:c1"]
	d.mu.Unlock()
	if !stillLive || l.conn != owner || !l.mcpPush {
		t.Fatalf("unregister from a different conn touched the session: live=%v, %+v", stillLive, l)
	}
}

func TestFromClientExactMatch(t *testing.T) {
	claude := claudeFake(adapter.ChannelPull)
	codex := codexFake(adapter.ChannelPull)
	codex.clientNames = []string{"codex-mcp-client"}
	r := adapter.NewRegistry(claude, codex)

	if _, ok := r.FromClient("claude-ai"); ok {
		t.Fatal("claude-ai matched the claude-code fake")
	}
	got, ok := r.FromClient("codex-mcp-client")
	if !ok || got.Name() != "codex" {
		t.Fatalf("FromClient(codex-mcp-client) = %v, %v", got, ok)
	}
	got, ok = r.FromClient("claude-code")
	if !ok || got.Name() != "claude-code" {
		t.Fatalf("FromClient(claude-code) = %v, %v", got, ok)
	}
}

func TestSlugBase(t *testing.T) {
	cases := map[string]string{
		"/src/api":  "api",
		"/src/api-": "api",
		"/a  b":     "a-b",
		"/日本語":      "session",
		"/":         "session",
		"":          "session",
	}
	for cwd, want := range cases {
		if got := slugBase(cwd); got != want {
			t.Errorf("slugBase(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestBuildNameNoTrailingDashAfterTruncation(t *testing.T) {
	cases := []struct {
		base, short string
		n           int
		want        string
	}{
		{"aaaa-bbbb", strings.Repeat("s", 34), 1, "aaaa-" + strings.Repeat("s", 34)},
		{"aaaa-bbbb", strings.Repeat("s", 32), 2, "aaaa-" + strings.Repeat("s", 32) + "-2"},
	}
	for _, c := range cases {
		got := buildName(c.base, c.short, c.n)
		if got != c.want {
			t.Errorf("buildName(%q, %q, %d) = %q, want %q", c.base, c.short, c.n, got, c.want)
		}
		if len(got) > 40 {
			t.Errorf("buildName(%q, %q, %d) = %q, too long (%d)", c.base, c.short, c.n, got, len(got))
		}
	}
}
