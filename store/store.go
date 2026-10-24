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

// AcceptOutcome is what Accept did with a message received from another machine.
type AcceptOutcome int

const (
	Inserted  AcceptOutcome = iota + 1 // stored, with its tombstone
	Duplicate                          // already received from the same sender for the same recipient; nothing changed
	Conflict                           // the id is taken by another sender, another recipient or a local message; nothing changed
)

// Store keeps mailboxes, the outbox, remembered sessions and small key-value blobs.
type Store interface {
	PutMessage(m message.Message, mailbox string, held bool) (inserted bool, err error)
	Pending(mailbox string) ([]message.Message, error) // unread, not held, oldest first
	MarkRead(ids []string, at time.Time) error
	Has(id string) (bool, error)
	MessageByID(id string) (m *message.Message, mailbox string, err error)                                   // nil, "", nil when absent
	Accept(m message.Message, mailbox string, held bool, sender string, at time.Time) (AcceptOutcome, error) // message row and tombstone in one transaction
	Received(id string) (sender, recipient string, ok bool, err error)                                       // the tombstone; it survives MarkRead and Drop
	PruneReceived(before time.Time) (int, error)
	Held() ([]Held, error)
	IsHeld(id string) (bool, error)                      // false, nil when absent or not held
	ReleaseHeld(ids []string) ([]message.Message, error) // one transaction; returns only ids that were actually held
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
