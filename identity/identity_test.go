package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
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
	if err != nil || got.PairingCode() != r.PairingCode() || len(r.PairingCode()) != 14 {
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

// LoadKeys must reject a truncated or garbage-length sign key, not misinterpret it.
func TestLoadKeysRejectsCorruptSign(t *testing.T) {
	k, _ := GenerateKeys()
	p := filepath.Join(t.TempDir(), "machine.key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate the base64 "sign" field so the decoded seed is short, not 32 bytes.
	corrupt := strings.Replace(string(b), `"sign":"`, `"sign":"AAAA`, 1)
	if err := os.WriteFile(p, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(p); err == nil {
		t.Fatal("corrupt sign key accepted")
	}
}

// LoadKeys must reject a wrong-length enc key.
func TestLoadKeysRejectsCorruptEnc(t *testing.T) {
	k, _ := GenerateKeys()
	p := filepath.Join(t.TempDir(), "machine.key")
	if err := k.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(b), `"enc":"`, `"enc":"AAAA`, 1)
	if err := os.WriteFile(p, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(p); err == nil {
		t.Fatal("corrupt enc key accepted")
	}
}

// Cert.Verify and Block.Verify must error, not panic, on a short founder key.
func TestVerifyRejectsShortFounderKey(t *testing.T) {
	fpub, fpriv := founder(t)
	k, _ := GenerateKeys()
	now := time.Unix(1_800_000_000, 0)
	c := Cert{V: 1, Team: TeamID(fpub), Key: k.Public(), Enc: k.EncPublic(), Person: "sam", Machine: "xps", Issued: now.Unix()}
	c.Sign(fpriv)
	if err := c.Verify(fpub[:8], now); err == nil {
		t.Fatal("short founder key accepted by Cert.Verify")
	}
	blk := Block{Team: TeamID(fpub), Key: k.Public(), Reason: "lost laptop", TS: 1}
	blk.Sign(fpriv)
	if err := blk.Verify(fpub[:8]); err == nil {
		t.Fatal("short founder key accepted by Block.Verify")
	}
}

// DecodeBundle must reject a short Founder key.
func TestDecodeBundleRejectsShortFounder(t *testing.T) {
	b := Bundle{V: 1, Founder: []byte{1, 2, 3}}
	if _, err := DecodeBundle(b.Encode()); err == nil {
		t.Fatal("short founder key accepted")
	}
}

// DecodeJoinRequest must validate Person/Machine slugs and Key/Enc lengths.
func TestDecodeJoinRequestValidatesFields(t *testing.T) {
	k, _ := GenerateKeys()
	bad := JoinRequest{V: 1, Person: "Not_A_Slug!", Machine: "xps", Key: k.Public(), Enc: k.EncPublic(), Nonce: []byte("12345678")}
	if _, err := DecodeJoinRequest(bad.Encode()); err == nil {
		t.Fatal("bad person slug accepted")
	}
	bad = JoinRequest{V: 1, Person: "sam", Machine: "Not_A_Slug!", Key: k.Public(), Enc: k.EncPublic(), Nonce: []byte("12345678")}
	if _, err := DecodeJoinRequest(bad.Encode()); err == nil {
		t.Fatal("bad machine slug accepted")
	}
	bad = JoinRequest{V: 1, Person: "sam", Machine: "xps", Key: []byte{1, 2, 3}, Enc: k.EncPublic(), Nonce: []byte("12345678")}
	if _, err := DecodeJoinRequest(bad.Encode()); err == nil {
		t.Fatal("bad key length accepted")
	}
	bad = JoinRequest{V: 1, Person: "sam", Machine: "xps", Key: k.Public(), Enc: []byte{1, 2, 3}, Nonce: []byte("12345678")}
	if _, err := DecodeJoinRequest(bad.Encode()); err == nil {
		t.Fatal("bad enc length accepted")
	}
}

// PairingCode must not collide with TeamCode on related inputs, and must change with the nonce.
func TestPairingCodeDistinctFromTeamCode(t *testing.T) {
	fpub, _ := founder(t)
	k, _ := GenerateKeys()
	r := JoinRequest{V: 1, Person: "sam", Machine: "xps", Key: k.Public(), Enc: k.EncPublic(), Nonce: []byte("12345678")}
	if r.PairingCode() == TeamCode(fpub) {
		t.Fatalf("pairing code collides with team code: %q", r.PairingCode())
	}
	if r.PairingCode() == TeamCode(k.Public()) {
		t.Fatalf("pairing code collides with team code over the request's own key: %q", r.PairingCode())
	}
	r2 := r
	r2.Nonce = []byte("87654321")
	if r.PairingCode() == r2.PairingCode() {
		t.Fatal("pairing code did not change with the nonce")
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
