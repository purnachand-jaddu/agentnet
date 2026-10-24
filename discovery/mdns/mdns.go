// Package mdns discovers agentnet peers over LAN multicast DNS, filtered by a team tag so the team id itself isn't broadcast.
package mdns

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	hmdns "github.com/hashicorp/mdns"

	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/internal/netaddr"
)

// service is the mDNS service type agentnet announces and browses.
const service = "_agentnet._tcp"

// tagMsg is the fixed message HMAC'd with the team id to make the team tag.
const tagMsg = "agentnet-mdns"

// defaultInterval is how often Browse re-queries when Interval is unset.
const defaultInterval = 30 * time.Second

// queryTimeout is how long each mDNS query waits for responses.
const queryTimeout = time.Second

// maxTXTBytes is a DNS TXT record string's byte limit (RFC 1035 character-string).
const maxTXTBytes = 255

// discardLogger silences hashicorp/mdns's std-log output so the daemon log isn't spammed.
var discardLogger = log.New(io.Discard, "", 0)

// Discoverer finds agentnet peers over mDNS.
type Discoverer struct {
	Self     []byte                   // if set, Browse drops entries whose key equals Self
	Interval time.Duration            // Browse's query interval; defaultInterval if zero
	IPs      func() ([]net.IP, error) // LAN IPv4 addresses Announce advertises; netaddr.LocalIPv4 if nil
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

// parseTXT extracts the k=, t= and a= values from an entry's TXT fields.
func parseTXT(fields []string) (k, t, a string) {
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "k="):
			k = f[2:]
		case strings.HasPrefix(f, "t="):
			t = f[2:]
		case strings.HasPrefix(f, "a="):
			a = f[2:]
		}
	}
	return k, t, a
}

// formatAddrs builds the "a=ip1,ip2,..." TXT value from ips, dropping non-IPv4 entries and truncating
// the list so the string (including the "a=" prefix) stays within maxTXTBytes.
func formatAddrs(ips []net.IP) string {
	var b strings.Builder
	b.WriteString("a=")
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		add := ip4.String()
		if b.Len() > 2 {
			add = "," + add
		}
		if b.Len()+len(add) > maxTXTBytes {
			break
		}
		b.WriteString(add)
	}
	return b.String()
}

// parseAddrs4 splits a comma-separated a= TXT value into valid IPv4 addresses, dropping anything else.
func parseAddrs4(a string) []net.IP {
	if a == "" {
		return nil
	}
	var ips []net.IP
	for _, s := range strings.Split(a, ",") {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			ips = append(ips, ip4)
		}
	}
	return ips
}

// containsIP reports whether ips already has ip.
func containsIP(ips []net.IP, ip net.IP) bool {
	for _, x := range ips {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

// Announce starts an mDNS responder advertising key on port for team, shutting down once ctx is done.
// It advertises this machine's own LAN IPv4 addresses (via IPs, or netaddr.LocalIPv4 if unset) rather
// than letting hashicorp/mdns resolve os.Hostname() itself, which can land on a loopback /etc/hosts entry.
func (d *Discoverer) Announce(ctx context.Context, key []byte, team string, port int) error {
	ipsFn := d.IPs
	if ipsFn == nil {
		ipsFn = netaddr.LocalIPv4
	}
	ips, err := ipsFn()
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("no LAN IPv4 address to announce")
	}

	svc, err := newZone(key, team, port, ips)
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

// newZone builds the mDNS service record for key on port for team, advertising ips.
func newZone(key []byte, team string, port int, ips []net.IP) (*hmdns.MDNSService, error) {
	instance := base64.RawURLEncoding.EncodeToString(key)
	if len(instance) > 12 {
		instance = instance[:12]
	}
	txt := []string{
		"k=" + base64.RawURLEncoding.EncodeToString(key),
		"t=" + tag(team),
		formatAddrs(ips),
	}
	return hmdns.NewMDNSService(instance, service, "", "", port, ips, txt)
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
	entries := make(chan *hmdns.ServiceEntry, 256)
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

// report calls found for e if its t= tag matches want and its k= decodes to a 32-byte key other than Self.
// Addrs carries every address from the a= TXT field plus e.AddrV4 as a fallback (deduped): hashicorp/mdns
// v1.0.7's client keeps only the LAST A record it sees, so on a multi-NIC host (Docker, a VPN) AddrV4 alone
// can be the wrong interface; the a= TXT value is this package's own record of every address it announced on.
func (d *Discoverer) report(e *hmdns.ServiceEntry, want string, found func(discovery.Found)) {
	k, t, a := parseTXT(e.InfoFields)
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

	ips := parseAddrs4(a)
	if e.AddrV4 != nil && !containsIP(ips, e.AddrV4) {
		ips = append(ips, e.AddrV4)
	}
	if len(ips) == 0 {
		return
	}

	addrs := make([]string, len(ips))
	for i, ip := range ips {
		addrs[i] = fmt.Sprintf("lan://%s:%d", ip, e.Port)
	}
	found(discovery.Found{Key: key, Addrs: addrs})
}
