// Package sqlite is the SQLite-backed implementation of store.Store.
package sqlite

import (
	"database/sql"
	"encoding/json"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY, mailbox TEXT NOT NULL, msg TEXT NOT NULL,
  held INTEGER NOT NULL DEFAULT 0, read_at INTEGER, seq INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS messages_mailbox ON messages(mailbox, read_at, held, seq);
CREATE TABLE IF NOT EXISTS outbox (
  id TEXT PRIMARY KEY, peer TEXT NOT NULL, msg TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  name TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, agent TEXT NOT NULL,
  cwd TEXT NOT NULL, last_seen INTEGER NOT NULL, kept INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (ns TEXT NOT NULL, key TEXT NOT NULL, val BLOB NOT NULL, PRIMARY KEY (ns, key));
`

// Store is a store.Store backed by a SQLite file.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite store at path, with mode 0600.
func Open(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func toMillis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// PutMessage inserts m into mailbox, ignoring a duplicate id.
func (s *Store) PutMessage(m message.Message, mailbox string, held bool) (bool, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO messages (id, mailbox, msg, held, seq)
		 VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(seq),0)+1 FROM messages))`,
		m.ID, mailbox, body, held)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Pending returns mailbox's unread, unheld messages, oldest first.
func (s *Store) Pending(mailbox string) ([]message.Message, error) {
	rows, err := s.db.Query(
		`SELECT msg FROM messages WHERE mailbox = ? AND read_at IS NULL AND held = 0 ORDER BY seq`,
		mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]message.Message, error) {
	var out []message.Message
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var m message.Message
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkRead marks ids as read at the given time.
func (s *Store) MarkRead(ids []string, at time.Time) error {
	stmt, err := s.db.Prepare(`UPDATE messages SET read_at = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(toMillis(at), id); err != nil {
			return err
		}
	}
	return nil
}

// Has reports whether id has ever been stored.
func (s *Store) Has(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// Held returns every held message, with its mailbox.
func (s *Store) Held() ([]store.Held, error) {
	rows, err := s.db.Query(`SELECT msg, mailbox FROM messages WHERE held = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Held
	for rows.Next() {
		var body []byte
		var mailbox string
		if err := rows.Scan(&body, &mailbox); err != nil {
			return nil, err
		}
		var m message.Message
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		out = append(out, store.Held{Message: m, Mailbox: mailbox})
	}
	return out, rows.Err()
}

// Release clears the held flag on ids.
func (s *Store) Release(ids []string) error {
	stmt, err := s.db.Prepare(`UPDATE messages SET held = 0 WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return nil
}

// Drop deletes ids from messages.
func (s *Store) Drop(ids []string) error {
	stmt, err := s.db.Prepare(`DELETE FROM messages WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return nil
}

// OutboxAdd queues m for peer at time at.
func (s *Store) OutboxAdd(peer string, m message.Message, at time.Time) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO outbox (id, peer, msg, at) VALUES (?, ?, ?, ?)`,
		m.ID, peer, body, toMillis(at))
	return err
}

// OutboxFor returns peer's queued messages older than olderThan, oldest first.
func (s *Store) OutboxFor(peer string, olderThan time.Time) ([]message.Message, error) {
	rows, err := s.db.Query(`SELECT msg FROM outbox WHERE peer = ? AND at <= ? ORDER BY at`,
		peer, toMillis(olderThan))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// OutboxPeers returns the distinct peers with queued messages.
func (s *Store) OutboxPeers() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT peer FROM outbox`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OutboxDone removes id from the outbox.
func (s *Store) OutboxDone(id string) error {
	_, err := s.db.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// OutboxPrune deletes queued messages older than before, returning the count removed.
func (s *Store) OutboxPrune(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM outbox WHERE at < ?`, toMillis(before))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// UpsertSession stores r, replacing any other session with the same key.
func (s *Store) UpsertSession(r store.SessionRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sessions WHERE key = ? AND name != ?`, r.Key, r.Name); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO sessions (name, key, agent, cwd, last_seen, kept) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET key = ?, agent = ?, cwd = ?, last_seen = ?, kept = ?`,
		r.Name, r.Key, r.Agent, r.Cwd, toMillis(r.LastSeen), r.Kept,
		r.Key, r.Agent, r.Cwd, toMillis(r.LastSeen), r.Kept); err != nil {
		return err
	}
	return tx.Commit()
}

func scanSession(row *sql.Row) (*store.SessionRecord, error) {
	var r store.SessionRecord
	var lastSeen int64
	err := row.Scan(&r.Name, &r.Key, &r.Agent, &r.Cwd, &lastSeen, &r.Kept)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.LastSeen = fromMillis(lastSeen)
	return &r, nil
}

// SessionByName returns the session named name, or nil if none.
func (s *Store) SessionByName(name string) (*store.SessionRecord, error) {
	row := s.db.QueryRow(`SELECT name, key, agent, cwd, last_seen, kept FROM sessions WHERE name = ?`, name)
	return scanSession(row)
}

// SessionByKey returns the session with key key, or nil if none.
func (s *Store) SessionByKey(key string) (*store.SessionRecord, error) {
	row := s.db.QueryRow(`SELECT name, key, agent, cwd, last_seen, kept FROM sessions WHERE key = ?`, key)
	return scanSession(row)
}

// ForgetSessions deletes non-kept sessions last seen before before, returning the count removed.
func (s *Store) ForgetSessions(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE kept = 0 AND last_seen < ?`, toMillis(before))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Put stores val under ns/key.
func (s *Store) Put(ns, key string, val []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO kv (ns, key, val) VALUES (?, ?, ?) ON CONFLICT(ns, key) DO UPDATE SET val = ?`,
		ns, key, val, val)
	return err
}

// Get returns the value at ns/key, or nil, nil if absent.
func (s *Store) Get(ns, key string) ([]byte, error) {
	var val []byte
	err := s.db.QueryRow(`SELECT val FROM kv WHERE ns = ? AND key = ?`, ns, key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return val, err
}

// List returns every key/value pair under ns.
func (s *Store) List(ns string) (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT key, val FROM kv WHERE ns = ?`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]byte)
	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, rows.Err()
}

// Delete removes ns/key.
func (s *Store) Delete(ns, key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE ns = ? AND key = ?`, ns, key)
	return err
}
