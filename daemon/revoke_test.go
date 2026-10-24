package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
	"github.com/purnachand-jaddu/agentnet/team"
)

// TestTeamChangedReloadsAndGossips proves "team_changed" picks up a block written out of band and
// spreads it to a third member (C) who never talked to the revoked machine (B) directly.
func TestTeamChangedReloadsAndGossips(t *testing.T) {
	f := newTeamFixture(t)
	a, _, _ := startPair(t, f)
	bk := peerKey(f.bKeys.Public())

	a.mu.Lock()
	_, seenBefore := a.peers[bk]
	a.mu.Unlock()
	if !seenBefore {
		t.Fatal("A should already know about B")
	}

	// Admit a third member C, and give A a fallback address to reach it (as discovery would).
	now := time.Now()
	cKeys, _ := identity.GenerateKeys()
	cBundle, err := team.Admit(f.aStore, f.fpriv, team.NewRequest(cKeys, "pat", "tp"), false, now)
	if err != nil {
		t.Fatal(err)
	}
	cStore := storetest.SQLite(t)
	cTeam, err := team.Accept(cStore, cKeys, cBundle, now)
	if err != nil {
		t.Fatal(err)
	}
	c := newNetDaemon(t, "pat", "tp", cStore, cTeam, cKeys, freePort(t))
	runDaemon(t, c)
	// C's own static discoverer (configured with no peers) never announces anything, so give A a
	// fallback hint to reach it, as mDNS discovery would once C's listener is actually up. B may log
	// a harmless "not for this machine" refusal too: A's fallback pool isn't keyed per-peer, so the
	// same race() send can also reach B's address while resolving where to reach C.
	var cAddrs []string
	waitUntil(t, 3*time.Second, "C is listening", func() bool {
		cAddrs = c.status().Addrs
		return len(cAddrs) > 0
	})
	a.found(discovery.Found{Addrs: cAddrs})

	// Revoke B out of band, as a separate `agentnet revoke` process would: its own *team.State loaded
	// from the same store A's daemon already has open.
	other, err := team.Load(f.aStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.Revoke(f.aStore, f.fpriv, other, f.bKeys.Public(), "revoked by founder", now); err != nil {
		t.Fatal(err)
	}

	if err := f.aTeam.Verify(f.bTeam.Self, now); err != nil {
		t.Fatal("sanity: A's live Team should still accept B's cert before team_changed")
	}

	call[struct{}](t, a, &fakeConn{id: 99}, "team_changed", nil)

	if err := f.aTeam.Verify(f.bTeam.Self, now); err == nil {
		t.Fatal("A's Team should reject B's cert after team_changed")
	}
	a.mu.Lock()
	_, stillThere := a.peers[bk]
	a.mu.Unlock()
	if stillThere {
		t.Fatal("team_changed should have dropped B from A's directory")
	}

	waitUntil(t, 5*time.Second, "C learns of B's block via the immediate gossip round", func() bool {
		return cTeam.Verify(f.bTeam.Self, time.Now()) != nil
	})
}

// TestBlockedPeerDroppedFromDirectoryAndPresenceRefused covers the merge-from-gossip path: once B
// merges a presence-carried block for A, it drops A from its directory and refuses further presence.
func TestBlockedPeerDroppedFromDirectoryAndPresenceRefused(t *testing.T) {
	f := newTeamFixture(t)
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)
	now := time.Now()
	ak := peerKey(f.aKeys.Public())

	if ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, now, now, "hello")); ack.State != message.StateDelivered {
		t.Fatalf("seed presence: %+v", ack)
	}
	b.mu.Lock()
	_, present := b.peers[ak]
	b.mu.Unlock()
	if !present {
		t.Fatal("B should have A in its directory after a valid presence")
	}

	blk := identity.Block{Team: identity.TeamID(f.bTeam.Founder), Key: f.aKeys.Public(), Reason: "revoked by founder", TS: now.Unix()}
	blk.Sign(f.fpriv)
	ts := now.Add(time.Second)
	payload, _ := json.Marshal(presence{Blocks: []identity.Block{blk}, TS: ts.UnixMilli()})
	env, err := envelope.Build("presence", message.NewID(), f.aKeys, f.aTeam.Self, f.bTeam.Self, payload, seal.HPKE{}, ts)
	if err != nil {
		t.Fatal(err)
	}
	// A's own presence carries the block that blocks A: Verify(e.Cert) must fail after the merge.
	if ack := b.handleEnvelope(context.Background(), env); ack.State != message.StateRefused {
		t.Fatalf("presence carrying its own sender's block: ack %+v, want refused", ack)
	}
	b.mu.Lock()
	_, stillPresent := b.peers[ak]
	b.mu.Unlock()
	if stillPresent {
		t.Fatal("B should have dropped A from its directory once the block merged")
	}

	ack2 := b.handleEnvelope(context.Background(), presenceFrom(t, f, now.Add(2*time.Second), now.Add(2*time.Second), "again"))
	if ack2.State != message.StateRefused {
		t.Fatalf("presence from now-blocked A: ack %+v, want refused", ack2)
	}
}

// TestTimeSkewIncrementsCounterAndPeersOpReportsIt proves a presence refused for being outside the
// time window is counted per sender key, and the "peers" op reports it, for doctor.
func TestTimeSkewIncrementsCounterAndPeersOpReportsIt(t *testing.T) {
	f := newTeamFixture(t)
	b := newNetDaemon(t, "sam", "xps", f.bStore, f.bTeam, f.bKeys, 0)

	stale := time.Now().Add(-10 * time.Minute) // outside the 5-minute default window
	ack := b.handleEnvelope(context.Background(), presenceFrom(t, f, stale, stale, "x"))
	if ack.State != message.StateRefused || ack.Note != "stale or replayed" {
		t.Fatalf("ack %+v, want refused/\"stale or replayed\"", ack)
	}

	ak := peerKey(f.aKeys.Public())
	res := b.peersOp()
	var found *PeerInfo
	for i := range res.Peers {
		if res.Peers[i].Key == ak {
			found = &res.Peers[i]
		}
	}
	if found == nil {
		t.Fatal("peers op should list A")
	}
	if found.TimeRefusals != 1 {
		t.Fatalf("TimeRefusals = %d, want 1", found.TimeRefusals)
	}
	if found.Online {
		t.Fatal("A should not be online: its only presence was refused")
	}
}
