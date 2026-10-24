// Package discovery hints at where other agentnet machines might be; identity is always proven later by envelope signatures.
package discovery

import "context"

// Found is a hint that a machine may be at these addresses; identity is proven by envelope signatures.
type Found struct {
	Key   []byte // may be empty for static peers
	Addrs []string
}

// Discoverer finds other agentnet machines on the network.
type Discoverer interface {
	// Announce advertises key on port for team until ctx is done.
	Announce(ctx context.Context, key []byte, team string, port int) error
	// Browse reports each discovered peer via found; it blocks until ctx is done.
	Browse(ctx context.Context, team string, found func(Found)) error
}
