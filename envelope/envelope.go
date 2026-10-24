// Package envelope signs and HPKE-seals messages between machines, and verifies and opens them on receipt.
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/team"
)

// maxSealed is the largest Sealed payload Open accepts.
const maxSealed = 1 << 20

// Envelope is a signed, HPKE-sealed message between two machines.
type Envelope struct {
	V      int           `json:"v"`
	Kind   string        `json:"kind"` // "msg" | "presence"
	ID     string        `json:"id"`   // for "msg": the message id; for "presence": a fresh ULID
	TS     int64         `json:"ts"`   // unix ms
	Cert   identity.Cert `json:"cert"` // sender
	To     []byte        `json:"to"`   // recipient machine key
	Sealed []byte        `json:"sealed"`
	Sig    []byte        `json:"sig,omitempty"`
}

// Ack reports what happened to one message, signed by the receiver.
type Ack struct {
	ID    string        `json:"id"`
	State message.State `json:"state"`
	Note  string        `json:"note"`
	TS    int64         `json:"ts"`
	Key   []byte        `json:"key"` // receiver
	Sig   []byte        `json:"sig,omitempty"`
}

// envelopeSignBytes is what the sender signs and the verifier checks, with e.Sig cleared.
func envelopeSignBytes(e Envelope) []byte {
	e.Sig = nil
	b, _ := json.Marshal(e) // Envelope always marshals; never fails
	return append([]byte("agentnet/envelope/v1\n"), b...)
}

// sealInfo is the HPKE info bound to kind+id and both parties' signing keys, so a ciphertext
// can't be re-signed and forwarded by another member as if it were theirs (surreptitious forwarding).
func sealInfo(kind, id string, senderKey, recipientKey []byte) []byte {
	return []byte("agentnet/v1/" + kind + "/" + id + "/" + base64.RawURLEncoding.EncodeToString(senderKey) + "/" + base64.RawURLEncoding.EncodeToString(recipientKey))
}

// Build seals payload to to's encryption key and signs the envelope with self.
func Build(kind, id string, self identity.Keys, selfCert identity.Cert, to identity.Cert, payload []byte, s seal.Sealer, now time.Time) (Envelope, error) {
	sealed, err := s.Seal(to.Enc, sealInfo(kind, id, selfCert.Key, to.Key), payload)
	if err != nil {
		return Envelope{}, err
	}
	e := Envelope{V: 1, Kind: kind, ID: id, TS: now.UnixMilli(), Cert: selfCert, To: to.Key, Sealed: sealed}
	e.Sig = ed25519.Sign(self.Sign, envelopeSignBytes(e))
	return e, nil
}

// Replay remembers keys (sender+id) for the time window.
type Replay struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]time.Time
}

// NewReplay makes a Replay tracker that forgets ids older than window.
func NewReplay(window time.Duration) *Replay {
	return &Replay{window: window, seen: map[string]time.Time{}}
}

// Check reports true the first time key is seen, prunes entries older than the window.
// ponytail: full prune per Check; prune on a ticker if n grows
func (r *Replay) Check(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.seen {
		if now.Sub(t) > r.window {
			delete(r.seen, k)
		}
	}
	if _, ok := r.seen[key]; ok {
		return false
	}
	r.seen[key] = now
	return true
}

// Verifier opens envelopes addressed to self.
type Verifier struct {
	Team   *team.State
	Replay *Replay
	Now    func() time.Time
	Window time.Duration // 5 minutes
}

// Open checks cert, signature, recipient, time window and replay, then decrypts.
func (v Verifier) Open(e Envelope, self identity.Keys, s seal.Sealer) ([]byte, error) {
	if e.V != 1 {
		return nil, fmt.Errorf("envelope: unsupported version %d", e.V)
	}
	if e.Kind != "msg" && e.Kind != "presence" {
		return nil, fmt.Errorf("envelope: unknown kind %q", e.Kind)
	}
	if len(e.Sealed) > maxSealed {
		return nil, fmt.Errorf("envelope: sealed payload too large (%d bytes)", len(e.Sealed))
	}
	if err := v.Team.Verify(e.Cert, v.Now()); err != nil {
		return nil, fmt.Errorf("envelope: sender cert: %w", err)
	}
	if len(e.Cert.Key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("envelope: bad sender key length %d", len(e.Cert.Key))
	}
	if !ed25519.Verify(e.Cert.Key, envelopeSignBytes(e), e.Sig) {
		return nil, errors.New("envelope: bad signature")
	}
	if !bytes.Equal(e.To, self.Public()) {
		return nil, errors.New("envelope: not for this machine")
	}
	now := v.Now()
	if d := now.Sub(time.UnixMilli(e.TS)); d > v.Window || d < -v.Window {
		return nil, errors.New("envelope: too old or from the future; check clocks")
	}
	if !message.ValidID(e.ID) {
		return nil, fmt.Errorf("envelope: bad id %q", e.ID)
	}
	replayKey := base64.RawURLEncoding.EncodeToString(e.Cert.Key) + "/" + e.ID
	if !v.Replay.Check(replayKey, now) {
		return nil, errors.New("envelope: replayed")
	}
	return s.Open(self.Enc, sealInfo(e.Kind, e.ID, e.Cert.Key, self.Public()), e.Sealed)
}

// ackSignBytes is what the receiver signs and the verifier checks, with a.Sig cleared.
func ackSignBytes(a Ack) []byte {
	a.Sig = nil
	b, _ := json.Marshal(a) // Ack always marshals; never fails
	return append([]byte("agentnet/ack/v1\n"), b...)
}

// SignAck sets a.Key to self's public key and signs a.
func SignAck(a *Ack, self identity.Keys) {
	a.Key = self.Public()
	a.Sig = ed25519.Sign(self.Sign, ackSignBytes(*a))
}

// VerifyAck checks a is for wantID, signed by from's key.
func VerifyAck(a Ack, from identity.Cert, wantID string) error {
	if a.ID != wantID {
		return fmt.Errorf("ack: id %q, want %q", a.ID, wantID)
	}
	if !bytes.Equal(a.Key, from.Key) {
		return errors.New("ack: key does not match sender")
	}
	if len(a.Key) != ed25519.PublicKeySize {
		return fmt.Errorf("ack: bad key length %d", len(a.Key))
	}
	if !ed25519.Verify(a.Key, ackSignBytes(a), a.Sig) {
		return errors.New("ack: bad signature")
	}
	return nil
}
