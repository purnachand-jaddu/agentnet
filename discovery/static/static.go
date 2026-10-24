// Package static is a Discoverer backed by a fixed list of configured peer addresses.
package static

import (
	"context"
	"net"
	"net/url"

	"github.com/purnachand-jaddu/agentnet/discovery"
)

// Discoverer reports a fixed, pre-configured list of peer addresses.
type Discoverer struct {
	peers []string
}

// New makes a static Discoverer reporting peers.
func New(peers []string) *Discoverer {
	return &Discoverer{peers: peers}
}

// Announce is a no-op: static peers need no announcing.
func (d *Discoverer) Announce(ctx context.Context, key []byte, team string, port int) error {
	return nil
}

// Browse reports each configured peer once, with an empty Key, then blocks until ctx is done.
func (d *Discoverer) Browse(ctx context.Context, team string, found func(discovery.Found)) error {
	for _, addr := range d.peers {
		if !validAddr(addr) {
			continue
		}
		found(discovery.Found{Addrs: []string{addr}})
	}
	<-ctx.Done()
	return nil
}

// validAddr reports whether addr is a well-formed lan://host:port address.
func validAddr(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "lan" || u.Host == "" {
		return false
	}
	_, _, err = net.SplitHostPort(u.Host)
	return err == nil
}
