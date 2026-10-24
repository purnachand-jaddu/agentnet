package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"

	"github.com/purnachand-jaddu/agentnet/message"
)

// AuditEntry is one line of the audit log: a message decision, never the body itself.
type AuditEntry struct {
	TS      int64         `json:"ts"`
	Dir     string        `json:"dir"` // "in" or "out"
	ID      string        `json:"id"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	State   message.State `json:"state"`
	BodySHA string        `json:"body_sha256,omitempty"` // absent when there's no trustworthy plaintext to hash
	Reason  string        `json:"reason,omitempty"`      // set for a refusal that never reached a decoded message
}

// Auditor records one AuditEntry per message decision.
type Auditor interface {
	Record(e AuditEntry)
}

// NoAudit discards every entry.
type NoAudit struct{}

func (NoAudit) Record(AuditEntry) {}

// FileAuditor appends JSON lines to a file opened once, mode 0600.
type FileAuditor struct {
	mu  sync.Mutex
	f   *os.File
	log func(format string, args ...any)
}

// NewFileAuditor opens (creating if needed) path for append-only writes, mode 0600.
func NewFileAuditor(path string, log func(format string, args ...any)) (*FileAuditor, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileAuditor{f: f, log: log}, nil
}

// Record appends e as one JSON line; an encode or write failure is logged, never returned.
func (a *FileAuditor) Record(e AuditEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		a.log("daemon: audit encode: %v", err)
		return
	}
	b = append(b, '\n')
	a.mu.Lock()
	_, err = a.f.Write(b)
	a.mu.Unlock()
	if err != nil {
		a.log("daemon: audit write: %v", err)
	}
}

// Close closes the underlying file.
func (a *FileAuditor) Close() error { return a.f.Close() }

// bodyHash is body's hex sha256, so an audit entry can prove what was sent without carrying it.
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// audit records one message decision; dir is "in" or "out".
func (d *Daemon) audit(dir, id, from, to string, st message.State, body string) {
	d.opts.Auditor.Record(AuditEntry{TS: d.opts.Now().UnixMilli(), Dir: dir, ID: id, From: from, To: to, State: st, BodySHA: bodyHash(body)})
}

// auditRefused records an inbound refusal from before decryption: no decoded message exists yet, so
// there's no trustworthy body to hash; id is the envelope id and from the claimed sender key.
func (d *Daemon) auditRefused(envelopeID, fromKey, reason string) {
	d.opts.Auditor.Record(AuditEntry{TS: d.opts.Now().UnixMilli(), Dir: "in", ID: envelopeID, From: fromKey, State: message.StateRefused, Reason: reason})
}
