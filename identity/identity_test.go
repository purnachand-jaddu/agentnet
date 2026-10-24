package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func founder(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestCertSignVerify(t *testing.T) {
	fpub, fpriv := founder(t)
	k, _ := GenerateKeys()
	now := time.Unix(1_800_000_000, 0)
	c := Cert{V: 1, Team: TeamID(fpub), Key: k.Public(), Enc: k.EncPublic(), Person: "sam", Machine: "xps", Issued: now.Unix()}
	c.Sign(fpriv)
	if err := c.Verify(fpub, now); err != nil {
		t.Fatal(err)
	}
	forged := c
	forged.Person = "alex"
	if err := forged.Verify(fpub, now); err == nil {
		t.Fatal("tampered cert accepted")
	}
	otherPub, _ := founder(t)
	if err := c.Verify(otherPub, now); err == nil {
		t.Fatal("cert from another founder accepted")
	}
	future := c
	future.Issued = now.Add(48 * time.Hour).Unix()
	future.Sign(fpriv)
	if err := future.Verify(fpub, now); err == nil {
		t.Fatal("cert from the future accepted")
	}
}

func TestBlock(t *testing.T) {
	fpub, fpriv := founder(t)
	k, _ := GenerateKeys()
	b := Block{Team: TeamID(fpub), Key: k.Public(), Reason: "lost laptop", TS: 1}
	b.Sign(fpriv)
	if err := b.Verify(fpub); err != nil {
		t.Fatal(err)
	}
	b.Reason = "x"
	if err := b.Verify(fpub); err == nil {
		t.Fatal("tampered block accepted")
	}
}

func TestJoinRoundTripAndCodes(t *testing.T) {
	k, _ := GenerateKeys()
	r := JoinRequest{V: 1, Person: "sam", Machine: "xps", Key: k.Public(), Enc: k.EncPublic(), Nonce: []byte("12345678")}
	got, err := DecodeJoinRequest(r.Encode())
	if err != nil || got.PairingCode() != r.PairingCode() || len(r.PairingCode()) != 11 {
		t.Fatalf("round trip: %v, code %q", err, r.PairingCode())
	}
	fpub, _ := founder(t)
	if len(TeamCode(fpub)) != 14 || TeamCode(fpub) == TeamCode(k.Public()) {
		t.Fatalf("team code %q", TeamCode(fpub))
	}
}

func TestKeysSaveLoad(t *testing.T) {
	k, _ := GenerateKeys()
	p := filepath.Join(t.TempDir(), "machine.key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	l, err := LoadKeys(p)
	if err != nil || !l.Public().Equal(k.Public()) {
		t.Fatalf("load: %v", err)
	}
}

// LoadKeys must refuse a key file whose mode allows group or other access.
func TestLoadKeysRefusesLooseMode(t *testing.T) {
	k, _ := GenerateKeys()
	p := filepath.Join(t.TempDir(), "machine.key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(p); err == nil {
		t.Fatal("loose-mode key file accepted")
	}
}

// Decode must reject a wrong prefix, oversized input and an unknown version.
func TestDecodeRejectsBadInput(t *testing.T) {
	if _, err := DecodeJoinRequest("wrong-prefix:abcd"); err == nil {
		t.Fatal("wrong prefix accepted")
	}
	huge := "agentnet-join1:" + string(make([]byte, 70*1024))
	if _, err := DecodeJoinRequest(huge); err == nil {
		t.Fatal("oversized input accepted")
	}
	r := JoinRequest{V: 2, Person: "sam", Machine: "xps", Nonce: make([]byte, 16)}
	if _, err := DecodeJoinRequest(r.Encode()); err == nil {
		t.Fatal("unknown version accepted")
	}
}

// Cert.Verify must reject a bad person slug.
func TestCertVerifyRejectsBadPersonSlug(t *testing.T) {
	fpub, fpriv := founder(t)
	k, _ := GenerateKeys()
	now := time.Unix(1_800_000_000, 0)
	c := Cert{V: 1, Team: TeamID(fpub), Key: k.Public(), Enc: k.EncPublic(), Person: "Not_A_Slug!", Machine: "xps", Issued: now.Unix()}
	c.Sign(fpriv)
	if err := c.Verify(fpub, now); err == nil {
		t.Fatal("bad person slug accepted")
	}
}

// Bundle must round trip through Encode/DecodeBundle.
func TestBundleRoundTrip(t *testing.T) {
	fpub, fpriv := founder(t)
	k, _ := GenerateKeys()
	now := time.Unix(1_800_000_000, 0)
	c := Cert{V: 1, Team: TeamID(fpub), Key: k.Public(), Enc: k.EncPublic(), Person: "sam", Machine: "xps", Issued: now.Unix()}
	c.Sign(fpriv)
	blk := Block{Team: TeamID(fpub), Key: k.Public(), Reason: "lost laptop", TS: 1}
	blk.Sign(fpriv)
	b := Bundle{V: 1, Founder: fpub, Cert: c, Members: []Cert{c}, Blocks: []Block{blk}}
	got, err := DecodeBundle(b.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Cert.Verify(fpub, now.Add(time.Hour)); err != nil {
		t.Fatalf("decoded cert: %v", err)
	}
	if len(got.Members) != 1 || len(got.Blocks) != 1 {
		t.Fatalf("bundle members/blocks: %+v", got)
	}
	if err := got.Blocks[0].Verify(fpub); err != nil {
		t.Fatalf("decoded block: %v", err)
	}
}
