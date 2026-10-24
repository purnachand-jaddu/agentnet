package store

import (
	"time"

	"github.com/purnachand-jaddu/agentnet/message"
)

// SessionRecord is a remembered agent session.
type SessionRecord struct {
	Name, Key, Agent, Cwd string
	LastSeen              time.Time
	Kept                  bool // the person's own mailbox (listen --as); never forgotten
}

// Held is a message parked in a mailbox awaiting release.
type Held struct {
	Message message.Message
	Mailbox string
}

// Store keeps mailboxes, the outbox, remembered sessions and small key-value blobs.
type Store interface {
	PutMessage(m message.Message, mailbox string, held bool) (inserted bool, err error)
	Pending(mailbox string) ([]message.Message, error) // unread, not held, oldest first
	MarkRead(ids []string, at time.Time) error
	Has(id string) (bool, error)
	Held() ([]Held, error)
	Release(ids []string) error
	Drop(ids []string) error
	Rehome(from, to string) error // moves from's unread mail (pending and held) to to; read mail stays put

	OutboxAdd(peer string, m message.Message, at time.Time) error
	OutboxFor(peer string, olderThan time.Time) ([]message.Message, error)
	OutboxPeers() ([]string, error)
	OutboxDone(id string) error
	OutboxPrune(before time.Time) (int, error)

	UpsertSession(r SessionRecord) error
	SessionByName(name string) (*SessionRecord, error)
	SessionByKey(key string) (*SessionRecord, error)
	ForgetSessions(before time.Time) (int, error)

	Put(ns, key string, val []byte) error
	Get(ns, key string) ([]byte, error) // nil, nil when absent
	List(ns string) (map[string][]byte, error)
	Delete(ns, key string) error

	Close() error
}
