package message

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

const MaxBody = 64 << 10

// Message is one note from an agent session to another.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	FromAgent string    `json:"from_agent,omitempty"`
	FromRepo  string    `json:"from_repo,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Body      string    `json:"body"`
	Wake      bool      `json:"wake"`
	SentAt    time.Time `json:"sent_at"`
}

// State is what happened to a message for one recipient.
type State string

const (
	StatePushed    State = "pushed"
	StateQueued    State = "queued"
	StateHeld      State = "held"
	StateRefused   State = "refused"
	StateRetry     State = "retry"
	StateDelivered State = "delivered"
)

// Receipt tells the sender what happened for one recipient.
type Receipt struct {
	To    string `json:"to"`
	State State  `json:"state"`
	Note  string `json:"note"`
}

func NewID() string { return ulid.MustNew(ulid.Now(), rand.Reader).String() }

func ValidID(s string) bool {
	id, err := ulid.ParseStrict(s)
	return err == nil && id.String() == s
}
