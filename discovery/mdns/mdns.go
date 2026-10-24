// Package mdns discovers agentnet peers over LAN multicast DNS, filtered by a team tag so the team id itself isn't broadcast.
package mdns

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	hmdns "github.com/hashicorp/mdns"

	"github.com/purnachand-jaddu/agentnet/discovery"
)

// service is the mDNS service type agentnet announces and browses.
const service = "_agentnet._tcp"

// tagMsg is the fixed message HMAC'd with the team id to make the team tag.
const tagMsg = "agentnet-mdns"

// defaultInterval is how often Browse re-queries when Interval is unset.
const defaultInterval = 30 * time.Second

// queryTimeout is how long each mDNS query waits for responses.
const queryTimeout = time.Second

// discardLogger silences hashicorp/mdns's std-log output so the daemon log isn't spammed.
var discardLogger = log.New(io.Discard, "", 0)

// Discoverer finds agentnet peers over mDNS.
type Discoverer struct {
	Self     []byte        // if set, Browse drops entries whose key equals Self
	Interval time.Duration // Browse's query interval; defaultInterval if zero
}

// New makes an mDNS Discoverer.
func New() *Discoverer {
	return &Discoverer{}
}

// tag derives the team's broadcast-safe fingerprint: HMAC-SHA256 keyed by team, truncated to 16 hex chars.
func tag(team string) string {
	h := hmac.New(sha256.New, []byte(team))
	h.Write([]byte(tagMsg))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// parseTXT extracts the k= and t= values from an entry's TXT fields.
func parseTXT(fields []string) (k, t string) {
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "k="); ok {
			k = v
		} else if v, ok := strings.CutPrefix(f, "t="); ok {
			t = v
		}
	}
	return k, t
}

// Announce starts an mDNS responder advertising key on port for team, shutting down once ctx is done.
func (d *Discoverer) Announce(ctx context.Context, key []byte, team string, port int) error {
	instance := base64.RawURLEncoding.EncodeToString(key)
	if len(instance) > 12 {
		instance = instance[:12]
	}
	txt := []string{
		"k=" + base64.RawURLEncoding.EncodeToString(key),
		"t=" + tag(team),
	}
	svc, err := hmdns.NewMDNSService(instance, service, "", "", port, nil, txt)
	if err != nil {
		return err
	}
	srv, err := hmdns.NewServer(&hmdns.Config{Zone: svc, Logger: discardLogger})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()
	return nil
}

// Browse queries for agentnet peers every Interval, reporting those whose team tag matches; it blocks until ctx is done.
func (d *Discoverer) Browse(ctx context.Context, team string, found func(discovery.Found)) error {
	interval := d.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	want := tag(team)

	d.query(ctx, want, found)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.query(ctx, want, found)
		}
	}
}

// query runs one mDNS lookup and reports matching entries; it blocks for up to queryTimeout.
func (d *Discoverer) query(ctx context.Context, want string, found func(discovery.Found)) {
	entries := make(chan *hmdns.ServiceEntry, 32)
	err := hmdns.QueryContext(ctx, &hmdns.QueryParam{
		Service:     service,
		Domain:      "local",
		Timeout:     queryTimeout,
		Entries:     entries,
		DisableIPv6: true,
		Logger:      discardLogger,
	})
	close(entries) // QueryContext has returned; nothing else sends on entries
	if err != nil {
		return
	}
	for e := range entries {
		d.report(e, want, found)
	}
}

// report calls found for e if its AddrV4 is set, its t= tag matches want, and its k= decodes to a 32-byte key other than Self.
func (d *Discoverer) report(e *hmdns.ServiceEntry, want string, found func(discovery.Found)) {
	if e.AddrV4 == nil {
		return
	}
	k, t := parseTXT(e.InfoFields)
	if t != want {
		return
	}
	key, err := base64.RawURLEncoding.DecodeString(k)
	if err != nil || len(key) != 32 {
		return
	}
	if d.Self != nil && bytes.Equal(key, d.Self) {
		return
	}
	found(discovery.Found{Key: key, Addrs: []string{fmt.Sprintf("lan://%s:%d", e.AddrV4, e.Port)}})
}
