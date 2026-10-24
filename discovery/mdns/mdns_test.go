package mdns

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

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
	k, tg := parseTXT([]string{"k=abc123", "t=deadbeef", "x=ignored"})
	if k != "abc123" || tg != "deadbeef" {
		t.Fatalf("got k=%q t=%q", k, tg)
	}
	k, tg = parseTXT(nil)
	if k != "" || tg != "" {
		t.Fatalf("want empty on no fields, got k=%q t=%q", k, tg)
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
	} else if len(f.Addrs) != 1 {
		t.Errorf("peer: want 1 addr, got %v", f.Addrs)
	}
}

func bytes32(b byte) []byte {
	k := make([]byte, 32)
	k[0] = b
	return k
}
