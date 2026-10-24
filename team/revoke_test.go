package team

import (
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

// TestRevokeRejectsCertAndMemberMergeRejectsToo covers the core promise: after Revoke, the founder's
// own Verify rejects the cert, and a member who only sees the gossiped Block via Merge rejects it too.
func TestRevokeRejectsCertAndMemberMergeRejectsToo(t *testing.T) {
	now := time.Now()
	_, fpriv := genFounder(t)
	fStore := storetest.SQLite(t)
	fKeys, _ := identity.GenerateKeys()
	fTeam, err := Create(fStore, fKeys, fpriv, "alex", "mbp", now)
	if err != nil {
		t.Fatal(err)
	}

	mKeys, _ := identity.GenerateKeys()
	bundle, err := Admit(fStore, fpriv, NewRequest(mKeys, "sam", "xps"), false, now)
	if err != nil {
		t.Fatal(err)
	}
	mCert := bundle.Cert

	if err := fTeam.Verify(mCert, now); err != nil {
		t.Fatalf("member cert should verify before revoke: %v", err)
	}

	blk, err := Revoke(fStore, fpriv, fTeam, mCert.Key, "revoked by founder", now)
	if err != nil {
		t.Fatal(err)
	}
	if blk.Reason != "revoked by founder" {
		t.Fatalf("block reason = %q", blk.Reason)
	}

	if err := fTeam.Verify(mCert, now); err == nil {
		t.Fatal("founder's own Verify should reject the revoked cert")
	}
	if reloaded, err := Load(fStore); err != nil {
		t.Fatal(err)
	} else if err := reloaded.Verify(mCert, now); err == nil {
		t.Fatal("the block should have been persisted")
	}

	// The member only learns of the block the way gossip would deliver it: via Merge.
	mStore := storetest.SQLite(t)
	mTeam, err := Accept(mStore, mKeys, bundle, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := mTeam.Verify(mCert, now); err != nil {
		t.Fatalf("member's own Verify should still pass before the block arrives: %v", err)
	}
	mTeam.Merge(nil, []identity.Block{blk}, now)
	if err := mTeam.Verify(mCert, now); err == nil {
		t.Fatal("member's Verify should reject the revoked cert after merging the gossiped block")
	}
}

// TestRevokeRefusesWrongFounderKey mirrors Admit's own-network check.
func TestRevokeRefusesWrongFounderKey(t *testing.T) {
	now := time.Now()
	_, fpriv := genFounder(t)
	fStore := storetest.SQLite(t)
	fKeys, _ := identity.GenerateKeys()
	fTeam, err := Create(fStore, fKeys, fpriv, "alex", "mbp", now)
	if err != nil {
		t.Fatal(err)
	}

	_, otherPriv := genFounder(t)
	mKeys, _ := identity.GenerateKeys()
	if _, err := Revoke(fStore, otherPriv, fTeam, mKeys.Public(), "x", now); err == nil {
		t.Fatal("revoke with the wrong founder key should fail")
	}
}

// TestReloadPicksUpOutOfBandBlock proves Reload lets a long-lived *State (like the daemon's) see a
// change another process made to the same store, in place, without replacing the pointer.
func TestReloadPicksUpOutOfBandBlock(t *testing.T) {
	now := time.Now()
	_, fpriv := genFounder(t)
	s := storetest.SQLite(t)
	fKeys, _ := identity.GenerateKeys()
	live, err := Create(s, fKeys, fpriv, "alex", "mbp", now)
	if err != nil {
		t.Fatal(err)
	}

	mKeys, _ := identity.GenerateKeys()
	bundle, err := Admit(s, fpriv, NewRequest(mKeys, "sam", "xps"), false, now)
	if err != nil {
		t.Fatal(err)
	}

	// A separate *State (as if another process ran `agentnet revoke`) writes the block to the same store.
	other, err := Load(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Revoke(s, fpriv, other, bundle.Cert.Key, "x", now); err != nil {
		t.Fatal(err)
	}

	if err := live.Verify(bundle.Cert, now); err != nil {
		t.Fatal("live should still accept the cert before Reload")
	}
	if err := live.Reload(s); err != nil {
		t.Fatal(err)
	}
	if err := live.Verify(bundle.Cert, now); err == nil {
		t.Fatal("live should reject the cert after Reload picks up the out-of-band block")
	}
}
