package mdns

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	hmdns "github.com/hashicorp/mdns"

	"github.com/purnachand-jaddu/agentnet/discovery"
)

func TestTag(t *testing.T) {
	a := tag("team-a")
	b := tag("team-b")
	if a == b {
		t.Fatal("different teams produced the same tag")
	}
	if a != tag("team-a") {
		t.Fatal("tag is not deterministic")
	}
	if len(a) != 16 {
		t.Fatalf("want 16 hex chars, got %d", len(a))
	}
}

func TestParseTXT(t *testing.T) {
	k, tg, a := parseTXT([]string{"k=abc123", "t=deadbeef", "a=10.0.0.5,192.168.1.20", "x=ignored"})
	if k != "abc123" || tg != "deadbeef" || a != "10.0.0.5,192.168.1.20" {
		t.Fatalf("got k=%q t=%q a=%q", k, tg, a)
	}
	k, tg, a = parseTXT(nil)
	if k != "" || tg != "" || a != "" {
		t.Fatalf("want empty on no fields, got k=%q t=%q a=%q", k, tg, a)
	}
}

// TestReport is a deterministic, non-networked check of the entry filter.
func TestReport(t *testing.T) {
	goodKey := bytes32(5)
	selfKey := bytes32(9)
	wantTag := tag("team-a")
	goodTXT := []string{"k=" + base64.RawURLEncoding.EncodeToString(goodKey), "t=" + wantTag}

	cases := []struct {
		name string
		self []byte
		e    *hmdns.ServiceEntry
		want bool
	}{
		{"wrong tag", nil, &hmdns.ServiceEntry{AddrV4: net.ParseIP("10.0.0.1"), Port: 4000,
			InfoFields: []string{"k=" + base64.RawURLEncoding.EncodeToString(goodKey), "t=" + tag("team-b")}}, false},
		{"bad key", nil, &hmdns.ServiceEntry{AddrV4: net.ParseIP("10.0.0.1"), Port: 4000,
			InfoFields: []string{"k=not-valid-base64!!", "t=" + wantTag}}, false},
		{"short key", nil, &hmdns.ServiceEntry{AddrV4: net.ParseIP("10.0.0.1"), Port: 4000,
			InfoFields: []string{"k=" + base64.RawURLEncoding.EncodeToString([]byte("too-short")), "t=" + wantTag}}, false},
		{"self key", selfKey, &hmdns.ServiceEntry{AddrV4: net.ParseIP("10.0.0.1"), Port: 4000,
			InfoFields: []string{"k=" + base64.RawURLEncoding.EncodeToString(selfKey), "t=" + wantTag}}, false},
		{"nil AddrV4", nil, &hmdns.ServiceEntry{AddrV4: nil, Port: 4000, InfoFields: goodTXT}, false},
		{"match", nil, &hmdns.ServiceEntry{AddrV4: net.ParseIP("10.0.0.1"), Port: 4000, InfoFields: goodTXT}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Discoverer{Self: c.self}
			var got *discovery.Found
			d.report(c.e, wantTag, func(f discovery.Found) { got = &f })

			if c.want != (got != nil) {
				t.Fatalf("report called = %v, want %v", got != nil, c.want)
			}
			if c.want {
				if !bytes.Equal(got.Key, goodKey) {
					t.Errorf("Key = %x, want %x", got.Key, goodKey)
				}
				if len(got.Addrs) != 1 || got.Addrs[0] != "lan://10.0.0.1:4000" {
					t.Errorf("Addrs = %v, want [lan://10.0.0.1:4000]", got.Addrs)
				}
			}
		})
	}
}

// TestNewZoneAdvertisesInjectedIPs guards against NewMDNSService falling back to os.Hostname()
// resolution (which can land on a loopback /etc/hosts entry): the service must carry exactly the
// IPs we hand it, never a 127/8 address.
func TestNewZoneAdvertisesInjectedIPs(t *testing.T) {
	ips := []net.IP{net.ParseIP("10.1.2.3"), net.ParseIP("10.1.2.4")}
	svc, err := newZone(bytes32(1), "team-a", 4000, ips)
	if err != nil {
		t.Fatalf("newZone: %v", err)
	}
	if len(svc.IPs) != len(ips) {
		t.Fatalf("got %d IPs, want %d: %v", len(svc.IPs), len(ips), svc.IPs)
	}
	for i, want := range ips {
		if !svc.IPs[i].Equal(want) {
			t.Errorf("IP %d = %v, want %v", i, svc.IPs[i], want)
		}
		if svc.IPs[i].IsLoopback() {
			t.Errorf("IP %d = %v is a loopback address", i, svc.IPs[i])
		}
	}
}

// TestNewZoneCarriesAllIPsInTXT guards against hashicorp/mdns v1.0.7's client keeping only the LAST
// A record it sees: every announced IP must also ride in the a= TXT value, not just svc.IPs.
func TestNewZoneCarriesAllIPsInTXT(t *testing.T) {
	ips := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.20")}
	svc, err := newZone(bytes32(1), "team-a", 4000, ips)
	if err != nil {
		t.Fatalf("newZone: %v", err)
	}
	_, _, a := parseTXT(svc.TXT)
	if want := "10.0.0.5,192.168.1.20"; a != want {
		t.Errorf("a= TXT = %q, want %q", a, want)
	}
}

// TestReportEmitsAllTXTAddrs checks report() reports every address in a=, deduped against AddrV4.
func TestReportEmitsAllTXTAddrs(t *testing.T) {
	goodKey := bytes32(5)
	wantTag := tag("team-a")
	e := &hmdns.ServiceEntry{
		AddrV4: net.ParseIP("10.0.0.5"), // same as the first a= address: must not be duplicated
		Port:   4000,
		InfoFields: []string{
			"k=" + base64.RawURLEncoding.EncodeToString(goodKey),
			"t=" + wantTag,
			"a=10.0.0.5,192.168.1.20",
		},
	}
	d := &Discoverer{}
	var got *discovery.Found
	d.report(e, wantTag, func(f discovery.Found) { got = &f })
	if got == nil {
		t.Fatal("want a report")
	}
	want := []string{"lan://10.0.0.5:4000", "lan://192.168.1.20:4000"}
	if len(got.Addrs) != len(want) {
		t.Fatalf("Addrs = %v, want %v", got.Addrs, want)
	}
	for i := range want {
		if got.Addrs[i] != want[i] {
			t.Errorf("Addrs[%d] = %q, want %q", i, got.Addrs[i], want[i])
		}
	}
}

// TestAnnounceErrorsWithNoIPs checks Announce refuses to start rather than silently falling back
// to hashicorp/mdns's own hostname resolution when no LAN IPv4 address is available.
func TestAnnounceErrorsWithNoIPs(t *testing.T) {
	d := New()
	d.IPs = func() ([]net.IP, error) { return nil, nil }
	err := d.Announce(context.Background(), bytes32(1), "team-a", 4000)
	if err == nil {
		t.Fatal("want an error when no LAN IPv4 address is available")
	}
	if err.Error() != "no LAN IPv4 address to announce" {
		t.Errorf("err = %q, want %q", err.Error(), "no LAN IPv4 address to announce")
	}
}

// TestAnnounceBrowse is a live mDNS test: it skips when multicast isn't available or AGENTNET_SKIP_MDNS=1.
func TestAnnounceBrowse(t *testing.T) {
	if os.Getenv("AGENTNET_SKIP_MDNS") == "1" {
		t.Skip("AGENTNET_SKIP_MDNS=1: skipping live mDNS test")
	}

	selfKey := bytes32(1)
	peerKey := bytes32(2)
	otherTeamKey := bytes32(3)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := New().Announce(ctx, selfKey, "team-a", 4101); err != nil {
		t.Fatalf("announce self: %v", err)
	}
	if err := New().Announce(ctx, peerKey, "team-a", 4102); err != nil {
		t.Fatalf("announce peer: %v", err)
	}
	if err := New().Announce(ctx, otherTeamKey, "team-b", 4103); err != nil {
		t.Fatalf("announce other team: %v", err)
	}

	browser := New()
	browser.Self = selfKey
	browser.Interval = 500 * time.Millisecond

	var mu sync.Mutex
	found := map[string]discovery.Found{}

	bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
	defer bcancel()
	done := make(chan error, 1)
	go func() {
		done <- browser.Browse(bctx, "team-a", func(f discovery.Found) {
			mu.Lock()
			defer mu.Unlock()
			found[string(f.Key)] = f
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("browse: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("browse did not return once ctx was done")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(found) == 0 {
		t.Skip("no mDNS entries observed within 5s: multicast likely unavailable in this environment")
	}
	if _, ok := found[string(selfKey)]; ok {
		t.Error("browse reported our own Self key")
	}
	if _, ok := found[string(otherTeamKey)]; ok {
		t.Error("browse reported a different team's announcement")
	}
	if f, ok := found[string(peerKey)]; !ok {
		t.Error("browse did not report the same-team peer")
	} else if len(f.Addrs) < 1 {
		t.Errorf("peer: want at least 1 addr, got %v", f.Addrs)
	}
}

func bytes32(b byte) []byte {
	k := make([]byte, 32)
	k[0] = b
	return k
}
