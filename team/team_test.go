package team

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
)

// genFounder makes a fresh ed25519 keypair standing in for a founder.
func genFounder(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestLoadNotInTeam(t *testing.T) {
	s := storetest.SQLite(t)
	st, err := Load(s)
	if err != nil || st != nil {
		t.Fatalf("Load = %v, %v, want nil, nil", st, err)
	}
}

func TestCreateAdmitAcceptRoundTrip(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	founderState, err := Create(fs, fkeys, fpriv, "alex", "mbp", now)
	if err != nil {
		t.Fatal(err)
	}
	if !founderState.Self.Admin {
		t.Fatal("founder's self cert should be admin")
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")
	if len(req.Nonce) != 16 {
		t.Fatalf("nonce len = %d, want 16", len(req.Nonce))
	}

	bundle, err := Admit(fs, fpriv, req, false, now)
	if err != nil {
		t.Fatal(err)
	}

	js := storetest.SQLite(t)
	joinerState, err := Accept(js, jkeys, bundle, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := joinerState.Verify(founderState.Self, now); err != nil {
		t.Fatalf("joiner can't verify founder's self cert: %v", err)
	}

	founderState2, err := Load(fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := founderState2.Verify(bundle.Cert, now); err != nil {
		t.Fatalf("founder can't verify joiner cert: %v", err)
	}
}

func TestAcceptRejectsWrongKey(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")
	bundle, err := Admit(fs, fpriv, req, false, now)
	if err != nil {
		t.Fatal(err)
	}

	otherKeys, _ := identity.GenerateKeys()
	js := storetest.SQLite(t)
	if _, err := Accept(js, otherKeys, bundle, now); err == nil {
		t.Fatal("accept with a key that doesn't match the bundle's cert should fail")
	}
}

func TestAttackerResignedBundleHasDifferentTeamCode(t *testing.T) {
	now := time.Now()
	fpub, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	fs := storetest.SQLite(t)
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")
	bundle, err := Admit(fs, fpriv, req, false, now)
	if err != nil {
		t.Fatal(err)
	}

	// An attacker intercepts the bundle and re-signs it with their own founder key.
	apub, apriv := genFounder(t)
	bundle.Founder = apub
	bundle.Cert.Team = identity.TeamID(apub)
	bundle.Cert.Sign(apriv)

	if identity.TeamCode(bundle.Founder) == identity.TeamCode(fpub) {
		t.Fatal("the attacker's team code should differ from the real founder's, so the human check catches it")
	}
}

func TestAdmitRefusesPersonTakenUnlessSamePerson(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "alex", "laptop2")
	if _, err := Admit(fs, fpriv, req, false, now); !errors.Is(err, ErrPersonTaken) {
		t.Fatalf("err = %v, want ErrPersonTaken", err)
	}
	if _, err := Admit(fs, fpriv, req, true, now); err != nil {
		t.Fatalf("samePerson admit should succeed: %v", err)
	}
}

func TestAdmitRefusesSameKeyClaimingADifferentIdentity(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")
	if _, err := Admit(fs, fpriv, req, false, now); err != nil {
		t.Fatal(err)
	}
	relabelled := req
	relabelled.Machine = "other"
	if _, err := Admit(fs, fpriv, relabelled, false, now); err == nil {
		t.Fatal("admitting the same key under a different claimed identity should fail")
	}
}

// Admitting the exact same request again (e.g. a lost bundle) must be idempotent: it hands back the
// existing cert rather than erroring or minting a new one.
func TestAdmitIsIdempotentForSameRequest(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")
	b1, err := Admit(fs, fpriv, req, false, now)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Admit(fs, fpriv, req, false, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-admitting the same request should succeed: %v", err)
	}
	if !bytes.Equal(b2.Cert.Sig, b1.Cert.Sig) {
		t.Fatal("idempotent re-issue should return the existing cert, not sign a new one")
	}
}

func TestAcceptRefusesDifferentFounder(t *testing.T) {
	now := time.Now()
	js := storetest.SQLite(t)
	jkeys, _ := identity.GenerateKeys()
	_, otherFounderPriv := genFounder(t)
	if _, err := Create(js, jkeys, otherFounderPriv, "sam", "xps", now); err != nil {
		t.Fatal(err)
	}

	fs := storetest.SQLite(t)
	fkeys, _ := identity.GenerateKeys()
	_, fpriv := genFounder(t)
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	req := NewRequest(jkeys, "sam", "xps")
	bundle, err := Admit(fs, fpriv, req, false, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Accept(js, jkeys, bundle, now); err == nil {
		t.Fatal("accept while already a member of another network should fail")
	}
}

func TestMergeKeepsLaterIssuedIgnoresStrangersAppliesBlocks(t *testing.T) {
	fpub, fpriv := genFounder(t)
	st := &State{Founder: fpub, Members: map[string]identity.Cert{}, Blocks: map[string]identity.Block{}}
	now := time.Unix(1000, 0)

	mkeys, _ := identity.GenerateKeys()
	older := identity.Cert{V: 1, Team: identity.TeamID(fpub), Key: mkeys.Public(), Enc: mkeys.EncPublic(), Person: "sam", Machine: "xps", Issued: 100}
	older.Sign(fpriv)
	newer := older
	newer.Issued = 200
	newer.Sign(fpriv)

	if !st.Merge([]identity.Cert{older}, nil, now) {
		t.Fatal("first merge should report changed")
	}
	if !st.Merge([]identity.Cert{newer}, nil, now) {
		t.Fatal("merging a later-Issued cert for the same key should report changed")
	}
	if st.Members[keyB64(mkeys.Public())].Issued != 200 {
		t.Fatal("merge should have kept the later Issued cert")
	}

	// A stranger-signed cert must be ignored.
	spub, spriv := genFounder(t)
	skeys, _ := identity.GenerateKeys()
	stranger := identity.Cert{V: 1, Team: identity.TeamID(spub), Key: skeys.Public(), Enc: skeys.EncPublic(), Person: "eve", Machine: "evil", Issued: 300}
	stranger.Sign(spriv)
	if st.Merge([]identity.Cert{stranger}, nil, now) {
		t.Fatal("a stranger-signed cert should be ignored")
	}

	blk := identity.Block{Team: identity.TeamID(fpub), Key: mkeys.Public(), Reason: "lost laptop", TS: 1}
	blk.Sign(fpriv)
	if !st.Merge(nil, []identity.Block{blk}, now) {
		t.Fatal("applying a valid block should report changed")
	}
	if err := st.Verify(newer, now); err == nil {
		t.Fatal("verify should reject a now-blocked machine")
	}
	if _, ok := st.Members[keyB64(mkeys.Public())]; ok {
		t.Fatal("merge should have removed the blocked member")
	}
}

// A block seen before a stale, still-validly-signed cert for the same key (e.g. arriving out of order via
// gossip) must stick: the cert must never resurrect the key in Members.
func TestMergeNeverResurrectsABlockedKey(t *testing.T) {
	fpub, fpriv := genFounder(t)
	st := &State{Founder: fpub, Members: map[string]identity.Cert{}, Blocks: map[string]identity.Block{}}
	now := time.Now()

	mkeys, _ := identity.GenerateKeys()
	cert := identity.Cert{V: 1, Team: identity.TeamID(fpub), Key: mkeys.Public(), Enc: mkeys.EncPublic(), Person: "sam", Machine: "xps", Issued: 100}
	cert.Sign(fpriv)

	blk := identity.Block{Team: identity.TeamID(fpub), Key: mkeys.Public(), Reason: "lost laptop", TS: 1}
	blk.Sign(fpriv)

	if !st.Merge(nil, []identity.Block{blk}, now) {
		t.Fatal("applying the block should report changed")
	}
	if st.Merge([]identity.Cert{cert}, nil, now) {
		t.Fatal("merging a cert for an already-blocked key should report no change")
	}
	if _, ok := st.Members[keyB64(mkeys.Public())]; ok {
		t.Fatal("blocked key resurrected in Members by a gossiped cert")
	}
}

// Admit must refuse a request for a key the founder has already blocked, even without going through Merge.
func TestAdmitRefusesBlockedKey(t *testing.T) {
	now := time.Now()
	fs := storetest.SQLite(t)
	fpub, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "alex", "mbp", now); err != nil {
		t.Fatal(err)
	}

	jkeys, _ := identity.GenerateKeys()
	req := NewRequest(jkeys, "sam", "xps")

	// Hand-sign a block for the joiner's key, as if the founder had already blocked it, and persist it.
	blk := identity.Block{Team: identity.TeamID(fpub), Key: jkeys.Public(), Reason: "pre-blocked", TS: 1}
	blk.Sign(fpriv)
	st, err := Load(fs)
	if err != nil {
		t.Fatal(err)
	}
	st.Merge(nil, []identity.Block{blk}, now)
	if err := st.save(fs); err != nil {
		t.Fatal(err)
	}

	if _, err := Admit(fs, fpriv, req, false, now); err == nil {
		t.Fatal("admit should refuse a blocked key")
	}
}

// Create must reject a person or machine name that isn't a valid slug.
func TestCreateRejectsBadPersonOrMachine(t *testing.T) {
	fs := storetest.SQLite(t)
	_, fpriv := genFounder(t)
	fkeys, _ := identity.GenerateKeys()
	if _, err := Create(fs, fkeys, fpriv, "Alex", "mbp", time.Now()); err == nil {
		t.Fatal("Create should reject a bad person name")
	}
	if _, err := Create(fs, fkeys, fpriv, "alex", "my mac", time.Now()); err == nil {
		t.Fatal("Create should reject a bad machine name")
	}
}
