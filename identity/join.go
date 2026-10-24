package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/purnachand-jaddu/agentnet/message"
)

const maxEnvelope = 64 * 1024

// JoinRequest is what a joining machine hands to the founder.
type JoinRequest struct {
	V       int    `json:"v"`
	Person  string `json:"person"`
	Machine string `json:"machine"`
	Key     []byte `json:"key"`
	Enc     []byte `json:"enc"`
	Nonce   []byte `json:"nonce"`
}

// PairingCode is "ABCD-EFGH-JKLM" from sha256("agentnet/pairing-code/v1"+Encode()): what the joiner reads to the founder.
func (r JoinRequest) PairingCode() string {
	return code12(sha256.Sum256(append([]byte("agentnet/pairing-code/v1"), []byte(r.Encode())...)))
}

// Encode is the wire form: "agentnet-join1:" + base64url(no padding)(json).
func (r JoinRequest) Encode() string { return encodeEnvelope("agentnet-join1:", r) }

// DecodeJoinRequest parses the wire form Encode produces.
func DecodeJoinRequest(s string) (JoinRequest, error) {
	b, err := decodeEnvelope(s, "agentnet-join1:")
	if err != nil {
		return JoinRequest{}, err
	}
	var r JoinRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return JoinRequest{}, fmt.Errorf("join request: bad json: %w", err)
	}
	if r.V != 1 {
		return JoinRequest{}, fmt.Errorf("join request: unsupported version %d", r.V)
	}
	if len(r.Nonce) < 8 {
		return JoinRequest{}, fmt.Errorf("join request: nonce too short")
	}
	if !message.Slug(r.Person) {
		return JoinRequest{}, fmt.Errorf("join request: bad person %q", r.Person)
	}
	if !message.Slug(r.Machine) {
		return JoinRequest{}, fmt.Errorf("join request: bad machine %q", r.Machine)
	}
	if len(r.Key) != ed25519.PublicKeySize {
		return JoinRequest{}, fmt.Errorf("join request: bad key length %d", len(r.Key))
	}
	if len(r.Enc) != 32 {
		return JoinRequest{}, fmt.Errorf("join request: bad enc key length %d", len(r.Enc))
	}
	return r, nil
}

// Bundle is what the founder hands back: the joiner's cert and the team.
type Bundle struct {
	V       int     `json:"v"`
	Founder []byte  `json:"founder"`
	Cert    Cert    `json:"cert"`
	Members []Cert  `json:"members"`
	Blocks  []Block `json:"blocks"`
}

// Encode is the wire form: "agentnet-bundle1:" + base64url(no padding)(json).
func (b Bundle) Encode() string { return encodeEnvelope("agentnet-bundle1:", b) }

// DecodeBundle parses the wire form Encode produces.
func DecodeBundle(s string) (Bundle, error) {
	data, err := decodeEnvelope(s, "agentnet-bundle1:")
	if err != nil {
		return Bundle{}, err
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("bundle: bad json: %w", err)
	}
	if b.V != 1 {
		return Bundle{}, fmt.Errorf("bundle: unsupported version %d", b.V)
	}
	if len(b.Founder) != ed25519.PublicKeySize {
		return Bundle{}, fmt.Errorf("bundle: bad founder key length %d", len(b.Founder))
	}
	return b, nil
}

// encodeEnvelope is prefix + base64url(no padding)(json(v)).
func encodeEnvelope(prefix string, v any) string {
	b, _ := json.Marshal(v) // v is always a plain struct; never fails
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// decodeEnvelope trims whitespace, checks the prefix, caps input at 64 KiB, and base64url-decodes the rest.
func decodeEnvelope(s, prefix string) ([]byte, error) {
	s = strings.TrimSpace(s)
	name := strings.TrimSuffix(prefix, ":")
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("%s: missing prefix", name)
	}
	if len(s) > maxEnvelope {
		return nil, fmt.Errorf("%s: input too large", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("%s: bad encoding: %w", name, err)
	}
	return b, nil
}
