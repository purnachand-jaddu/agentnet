package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/store/storetest"
	"github.com/purnachand-jaddu/agentnet/team"
)

// machine is one team member's local view: its keys, its own cert and its team state.
type machine struct {
	keys identity.Keys
	cert identity.Cert
	team *team.State
}

// genFounder makes a fresh ed25519 keypair standing in for a founder.
func genFounder(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// setup builds a founder-admin machine a, and two joined machines b and c, all on one founder.
func setup(t *testing.T, now time.Time) (fpub ed25519.PublicKey, fpriv ed25519.PrivateKey, a, b, c machine) {
	t.Helper()
	fpub, fpriv = genFounder(t)

	akeys, err := identity.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	fs := storetest.SQLite(t)
	aSt, err := team.Create(fs, akeys, fpriv, "alex", "mbp", now)
	if err != nil {
		t.Fatal(err)
	}

	bkeys, err := identity.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	bReq := team.NewRequest(bkeys, "sam", "xps")
	bBundle, err := team.Admit(fs, fpriv, bReq, false, now)
	if err != nil {
		t.Fatal(err)
	}

	ckeys, err := identity.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	cReq := team.NewRequest(ckeys, "pat", "thinkpad")
	cBundle, err := team.Admit(fs, fpriv, cReq, false, now)
	if err != nil {
		t.Fatal(err)
	}

	aSt, err = team.Load(fs) // refresh founder state so it knows about b and c
	if err != nil {
		t.Fatal(err)
	}

	bs := storetest.SQLite(t)
	bSt, err := team.Accept(bs, bkeys, bBundle, now)
	if err != nil {
		t.Fatal(err)
	}

	cs := storetest.SQLite(t)
	cSt, err := team.Accept(cs, ckeys, cBundle, now)
	if err != nil {
		t.Fatal(err)
	}

	return fpub, fpriv, machine{akeys, aSt.Self, aSt}, machine{bkeys, bSt.Self, bSt}, machine{ckeys, cSt.Self, cSt}
}

func newVerifier(st *team.State, now time.Time, window time.Duration) Verifier {
	return Verifier{Team: st, Replay: NewReplay(window), Now: func() time.Time { return now }, Window: window}
}

func TestOpenValidOnceThenReplayFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	got, err := v.Open(e, b.keys, s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("payload = %q, want %q", got, "hello")
	}

	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("opening the same envelope twice should fail (replay)")
	}
}

func TestTamperedSealedFailsSignature(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}
	e.Sealed[0] ^= 0xFF

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("tampered Sealed should fail signature verification")
	}
}

func TestAnotherRecipientFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, c := setup(t, now)
	s := seal.HPKE{}

	// Sealed and addressed to c, but b tries to open it.
	e, err := Build("msg", message.NewID(), a.keys, a.cert, c.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("opening an envelope addressed to another recipient should fail")
	}
}

func TestOldTSFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now.Add(-6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("an envelope 6 minutes old should fail the window check")
	}
}

func TestFutureTSFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("an envelope 6 minutes in the future should fail the window check")
	}
}

func TestCertFromAnotherFounderFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	_, otherFounderPriv := genFounder(t)
	forged := identity.Cert{
		V: 1, Team: identity.TeamID(otherFounderPriv.Public().(ed25519.PublicKey)),
		Key: a.keys.Public(), Enc: a.keys.EncPublic(), Person: "alex", Machine: "mbp", Issued: now.Unix(),
	}
	forged.Sign(otherFounderPriv)

	e, err := Build("msg", message.NewID(), a.keys, forged, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("a cert signed by another founder should fail")
	}
}

func TestBlockedSenderFails(t *testing.T) {
	now := time.Now()
	fpub, fpriv, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), b.keys, b.cert, a.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	blk := identity.Block{Team: identity.TeamID(fpub), Key: b.keys.Public(), Reason: "lost laptop", TS: now.Unix()}
	blk.Sign(fpriv)
	a.team.Merge(nil, []identity.Block{blk}, now)

	v := newVerifier(a.team, now, 5*time.Minute)
	if _, err := v.Open(e, a.keys, s); err == nil {
		t.Fatal("a blocked sender should fail")
	}
}

func TestBadKindFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("bogus", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("an unknown kind should fail")
	}
}

func TestNonULIDIDFails(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", "not-a-ulid", a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("a non-ULID id should fail")
	}
}

// A signature that verifies but whose Sealed was HPKE-sealed with a different info must still fail to open.
func TestSealedWithDifferentInfoFailsToOpen(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	id := message.NewID()
	wrongInfo := sealInfo("presence", id, a.cert.Key, b.cert.Key) // envelope below will claim kind "msg" for the same id
	sealed, err := s.Seal(b.cert.Enc, wrongInfo, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	e := Envelope{V: 1, Kind: "msg", ID: id, TS: now.UnixMilli(), Cert: a.cert, To: b.cert.Key, Sealed: sealed}
	e.Sig = ed25519.Sign(a.keys.Sign, envelopeSignBytes(e))

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(e, b.keys, s); err == nil {
		t.Fatal("a valid signature over a mismatched info should still fail to decrypt")
	}
}

func TestVerifyAck(t *testing.T) {
	now := time.Now()
	_, _, _, b, c := setup(t, now)

	id := message.NewID()
	ack := Ack{ID: id, State: message.StateDelivered, Note: "ok", TS: now.UnixMilli()}
	SignAck(&ack, b.keys)

	if err := VerifyAck(ack, b.cert, id); err != nil {
		t.Fatalf("VerifyAck of a valid ack: %v", err)
	}

	if err := VerifyAck(ack, b.cert, message.NewID()); err == nil {
		t.Fatal("VerifyAck should reject a mismatched id")
	}
	if err := VerifyAck(ack, c.cert, id); err == nil {
		t.Fatal("VerifyAck should reject an ack signed by a different key")
	}
}

// A member who observes another sender's Sealed bytes and re-signs them as its own must not be able
// to forward them: the HPKE info binds the ciphertext to the original sender's key, so decryption fails.
func TestForwardedSealedFailsEvenWhenResignedByAnotherMember(t *testing.T) {
	now := time.Now()
	_, _, a, b, c := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	// c (M) observes A's envelope to B and re-signs the same Sealed bytes as its own.
	forged := Envelope{V: e.V, Kind: e.Kind, ID: e.ID, TS: e.TS, Cert: c.cert, To: e.To, Sealed: e.Sealed}
	forged.Sig = ed25519.Sign(c.keys.Sign, envelopeSignBytes(forged))

	v := newVerifier(b.team, now, 5*time.Minute)
	if _, err := v.Open(forged, b.keys, s); err == nil {
		t.Fatal("re-signing another sender's Sealed bytes as a different sender should fail to decrypt")
	}
}

// The size guard runs before any cert or signature work, so an oversized Sealed fails immediately.
func TestOversizedSealedFailsAtTheTop(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e := Envelope{V: 1, Kind: "msg", ID: message.NewID(), TS: now.UnixMilli(), Cert: a.cert, To: b.cert.Key, Sealed: make([]byte, maxSealed+1)}
	// No valid Sig: the size guard must reject this before signature verification is ever reached.

	v := newVerifier(b.team, now, 5*time.Minute)
	_, err := v.Open(e, b.keys, s)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a size-guard error", err)
	}
}

// A tampered envelope must fail the same way every time it's opened, and never report "replayed":
// the signature check runs before the replay check, so a bad signature is never recorded as seen.
func TestTamperedEnvelopeFailsSameWayTwice(t *testing.T) {
	now := time.Now()
	_, _, a, b, _ := setup(t, now)
	s := seal.HPKE{}

	e, err := Build("msg", message.NewID(), a.keys, a.cert, b.cert, []byte("hello"), s, now)
	if err != nil {
		t.Fatal(err)
	}
	e.Sealed[0] ^= 0xFF

	v := newVerifier(b.team, now, 5*time.Minute)
	_, err1 := v.Open(e, b.keys, s)
	_, err2 := v.Open(e, b.keys, s)
	if err1 == nil || err2 == nil {
		t.Fatal("a tampered envelope should fail both times")
	}
	if strings.Contains(err1.Error(), "replayed") || strings.Contains(err2.Error(), "replayed") {
		t.Fatal("a tampered envelope should never be reported as replayed")
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("expected the same error both times, got %q then %q", err1, err2)
	}
}

// Replay is keyed by sender+id, so two different senders using the same id are not replays of each other.
func TestSameIDFromDifferentSendersIsNotAReplay(t *testing.T) {
	now := time.Now()
	_, _, a, b, c := setup(t, now)
	s := seal.HPKE{}
	id := message.NewID()

	e1, err := Build("msg", id, a.keys, a.cert, b.cert, []byte("from a"), s, now)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := Build("msg", id, c.keys, c.cert, b.cert, []byte("from c"), s, now)
	if err != nil {
		t.Fatal(err)
	}

	v := newVerifier(b.team, now, 5*time.Minute)
	got1, err := v.Open(e1, b.keys, s)
	if err != nil {
		t.Fatalf("first sender: %v", err)
	}
	if string(got1) != "from a" {
		t.Fatalf("got %q, want %q", got1, "from a")
	}
	got2, err := v.Open(e2, b.keys, s)
	if err != nil {
		t.Fatalf("second sender with same id: %v", err)
	}
	if string(got2) != "from c" {
		t.Fatalf("got %q, want %q", got2, "from c")
	}
}
