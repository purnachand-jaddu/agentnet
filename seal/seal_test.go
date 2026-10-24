package seal

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

func genKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	priv := genKey(t)
	s := HPKE{}
	info := []byte("agentnet/v1/msg/01ABC")
	plaintext := []byte("hello machine")

	ciphertext, err := s.Seal(priv.PublicKey().Bytes(), info, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open(priv, info, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	priv := genKey(t)
	other := genKey(t)
	s := HPKE{}
	info := []byte("agentnet/v1/msg/01ABC")

	ciphertext, err := s.Seal(priv.PublicKey().Bytes(), info, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(other, info, ciphertext); err == nil {
		t.Fatal("open with the wrong private key should fail")
	}
}

func TestOpenWithWrongInfoFails(t *testing.T) {
	priv := genKey(t)
	s := HPKE{}

	ciphertext, err := s.Seal(priv.PublicKey().Bytes(), []byte("agentnet/v1/msg/01ABC"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(priv, []byte("agentnet/v1/msg/01XYZ"), ciphertext); err == nil {
		t.Fatal("open with the wrong info should fail")
	}
}

func TestSealRejectsShortRecipientKey(t *testing.T) {
	s := HPKE{}
	if _, err := s.Seal([]byte("too short"), []byte("info"), []byte("payload")); err == nil {
		t.Fatal("seal with a non-32-byte recipient key should fail")
	}
}
