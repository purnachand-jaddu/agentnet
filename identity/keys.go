// Package identity holds machine keys, founder-signed certificates and revocation blocks, and join requests and bundles.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Keys is a machine's identity (ed25519) and encryption (X25519) keys.
type Keys struct {
	Sign ed25519.PrivateKey
	Enc  *ecdh.PrivateKey
}

// GenerateKeys makes a fresh signing and encryption keypair.
func GenerateKeys() (Keys, error) {
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Keys{}, err
	}
	enc, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Sign: sign, Enc: enc}, nil
}

// Public is the machine's ed25519 public key.
func (k Keys) Public() ed25519.PublicKey { return k.Sign.Public().(ed25519.PublicKey) }

// EncPublic is the machine's X25519 public key bytes.
func (k Keys) EncPublic() []byte { return k.Enc.PublicKey().Bytes() }

// keysFile is Keys as stored on disk.
type keysFile struct {
	Sign string `json:"sign"`
	Enc  string `json:"enc"`
}

// Save writes k to path as JSON, atomically (temp file + rename), mode 0600.
func (k Keys) Save(path string) error {
	f := keysFile{
		Sign: base64.RawURLEncoding.EncodeToString(k.Sign),
		Enc:  base64.RawURLEncoding.EncodeToString(k.Enc.Bytes()),
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keys-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// LoadKeys reads Keys from path, refusing a file whose mode allows group or other access.
func LoadKeys(path string) (Keys, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Keys{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return Keys{}, fmt.Errorf("%s: mode %03o allows group or other access, chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Keys{}, err
	}
	var f keysFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Keys{}, err
	}
	signBytes, err := base64.RawURLEncoding.DecodeString(f.Sign)
	if err != nil {
		return Keys{}, err
	}
	encBytes, err := base64.RawURLEncoding.DecodeString(f.Enc)
	if err != nil {
		return Keys{}, err
	}
	enc, err := ecdh.X25519().NewPrivateKey(encBytes)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Sign: ed25519.PrivateKey(signBytes), Enc: enc}, nil
}
