// Package seal HPKE-seals payloads to a recipient's X25519 public key and opens them with the matching private key.
package seal

import (
	"crypto/ecdh"
	"crypto/hpke"
	"fmt"
)

// Sealer encrypts to a recipient's public key and opens with the private key.
type Sealer interface {
	Seal(recipient []byte, info, plaintext []byte) ([]byte, error)
	Open(priv *ecdh.PrivateKey, info, ciphertext []byte) ([]byte, error)
}

// HPKE is DHKEM(X25519) + HKDF-SHA256 + ChaCha20-Poly1305 (RFC 9180, crypto/hpke).
type HPKE struct{}

// Seal encrypts plaintext to recipient's 32-byte X25519 public key, returning enc||ciphertext.
func (HPKE) Seal(recipient []byte, info, plaintext []byte) ([]byte, error) {
	if len(recipient) != 32 {
		return nil, fmt.Errorf("seal: recipient key is %d bytes, want 32", len(recipient))
	}
	pub, err := ecdh.X25519().NewPublicKey(recipient)
	if err != nil {
		return nil, err
	}
	pk, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return hpke.Seal(pk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, plaintext)
}

// Open decrypts ciphertext (enc||ciphertext) with priv.
func (HPKE) Open(priv *ecdh.PrivateKey, info, ciphertext []byte) ([]byte, error) {
	sk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return hpke.Open(sk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, ciphertext)
}
