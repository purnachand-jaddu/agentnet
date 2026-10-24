package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/purnachand-jaddu/agentnet/message"
)

// TeamID identifies a team by its founder key: base64url(sha256(pub))[:16].
func TeamID(founder ed25519.PublicKey) string {
	sum := sha256.Sum256(founder)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

var teamCodeAlphabet = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// TeamCode is the human-readable code the founder reads out and the joiner checks, e.g. "ABCD-EFGH-JKLM".
func TeamCode(founder ed25519.PublicKey) string {
	sum := sha256.Sum256(append([]byte("agentnet/team-code/v1"), founder...))
	s := teamCodeAlphabet.EncodeToString(sum[:])[:12]
	return s[:4] + "-" + s[4:8] + "-" + s[8:12]
}

// signPayload is the bytes a founder signs for kind: "agentnet/<kind>/v1\n" + JSON.
func signPayload(kind string, v any) []byte {
	b, _ := json.Marshal(v) // v is always a plain struct of strings/ints/bytes/bools; never fails
	return append([]byte("agentnet/"+kind+"/v1\n"), b...)
}

// Cert is a founder-signed statement that a machine belongs to the team.
type Cert struct {
	V       int    `json:"v"`
	Team    string `json:"team"`
	Key     []byte `json:"key"`
	Enc     []byte `json:"enc"`
	Person  string `json:"person"`
	Machine string `json:"machine"`
	Admin   bool   `json:"admin,omitempty"` // the founder's own machine
	Issued  int64  `json:"issued"`
	Sig     []byte `json:"sig,omitempty"`
}

// Sign signs c with the founder key, setting c.Sig.
func (c *Cert) Sign(founder ed25519.PrivateKey) {
	cp := *c
	cp.Sig = nil
	c.Sig = ed25519.Sign(founder, signPayload("cert", cp))
}

// Verify checks c's version, team, key lengths, slugs, issue time and signature against founder.
func (c Cert) Verify(founder ed25519.PublicKey, now time.Time) error {
	if len(founder) != ed25519.PublicKeySize {
		return fmt.Errorf("cert: bad founder key length %d", len(founder))
	}
	if c.V != 1 {
		return fmt.Errorf("cert: unsupported version %d", c.V)
	}
	if c.Team != TeamID(founder) {
		return fmt.Errorf("cert: team %q does not match founder", c.Team)
	}
	if len(c.Key) != ed25519.PublicKeySize {
		return fmt.Errorf("cert: bad key length %d", len(c.Key))
	}
	if len(c.Enc) != 32 {
		return fmt.Errorf("cert: bad enc key length %d", len(c.Enc))
	}
	if !message.Slug(c.Person) {
		return fmt.Errorf("cert: bad person %q", c.Person)
	}
	if !message.Slug(c.Machine) {
		return fmt.Errorf("cert: bad machine %q", c.Machine)
	}
	if time.Unix(c.Issued, 0).After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("cert: issued in the future")
	}
	cp := c
	cp.Sig = nil
	if !ed25519.Verify(founder, signPayload("cert", cp), c.Sig) {
		return fmt.Errorf("cert: bad signature")
	}
	return nil
}

// Block is a founder-signed revocation of a machine key.
type Block struct {
	Team   string `json:"team"`
	Key    []byte `json:"key"`
	Reason string `json:"reason"`
	TS     int64  `json:"ts"`
	Sig    []byte `json:"sig,omitempty"`
}

// Sign signs b with the founder key, setting b.Sig.
func (b *Block) Sign(founder ed25519.PrivateKey) {
	cp := *b
	cp.Sig = nil
	b.Sig = ed25519.Sign(founder, signPayload("block", cp))
}

// Verify checks b's team, key length and signature against founder.
func (b Block) Verify(founder ed25519.PublicKey) error {
	if len(founder) != ed25519.PublicKeySize {
		return fmt.Errorf("block: bad founder key length %d", len(founder))
	}
	if b.Team != TeamID(founder) {
		return fmt.Errorf("block: team %q does not match founder", b.Team)
	}
	if len(b.Key) != ed25519.PublicKeySize {
		return fmt.Errorf("block: bad key length %d", len(b.Key))
	}
	cp := b
	cp.Sig = nil
	if !ed25519.Verify(founder, signPayload("block", cp), b.Sig) {
		return fmt.Errorf("block: bad signature")
	}
	return nil
}
