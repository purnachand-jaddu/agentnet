// Package transport moves signed, sealed envelopes between machines; it never opens or verifies them.
package transport

import (
	"context"

	"github.com/purnachand-jaddu/agentnet/envelope"
)

// Handler answers one incoming envelope.
type Handler func(ctx context.Context, e envelope.Envelope) envelope.Ack

// Transport moves envelopes between machines; it is never trusted.
type Transport interface {
	Schemes() []string // address schemes it can dial, e.g. ["lan"]
	Listen(ctx context.Context, h Handler) (addrs []string, err error)
	Send(ctx context.Context, addr string, e envelope.Envelope) (envelope.Ack, error)
	Close() error
}
