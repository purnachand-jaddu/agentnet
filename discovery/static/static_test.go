package static

import (
	"context"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/discovery"
)

func TestBrowseReportsConfiguredPeers(t *testing.T) {
	d := New([]string{"lan://10.0.0.1:4000", "not-a-url", "lan://10.0.0.2:4001", "lan://missing-port"})

	var got []discovery.Found
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- d.Browse(ctx, "team-a", func(f discovery.Found) {
			got = append(got, f)
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("browse returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("browse did not return once ctx was done")
	}

	want := []string{"lan://10.0.0.1:4000", "lan://10.0.0.2:4001"}
	if len(got) != len(want) {
		t.Fatalf("got %d peers, want %d: %+v", len(got), len(want), got)
	}
	for i, f := range got {
		if len(f.Key) != 0 {
			t.Errorf("peer %d: want empty key, got %v", i, f.Key)
		}
		if len(f.Addrs) != 1 || f.Addrs[0] != want[i] {
			t.Errorf("peer %d: got addrs %v, want [%s]", i, f.Addrs, want[i])
		}
	}
}

func TestAnnounceIsNoop(t *testing.T) {
	d := New(nil)
	if err := d.Announce(context.Background(), []byte("key"), "team", 1234); err != nil {
		t.Fatalf("announce: %v", err)
	}
}
