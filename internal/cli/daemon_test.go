package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
)

// waitForSocket polls until path exists or t fails.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("socket never appeared")
}

func TestDaemonServesWhoAndReleasesLockOnShutdown(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("AGENTNET_HOME", home)

	done := make(chan int, 1)
	go func() { done <- Main([]string{"daemon"}, "test") }()

	sockPath := paths.Socket()
	waitForSocket(t, sockPath)

	client, err := ipc.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	var who daemon.WhoResult
	if err := client.Call(context.Background(), "who", daemon.WhoArgs{}, &who); err != nil {
		t.Fatalf("who: %v", err)
	}
	if len(who.Sessions) != 0 {
		t.Fatalf("who = %+v, want empty", who)
	}

	if err := client.Call(context.Background(), "shutdown", nil, nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	client.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("daemon exited %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit after shutdown")
	}

	f, err := acquireLock(paths.Lock(), false)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	f.Close()
}

func TestSecondNonServiceDaemonExitsZeroWhileFirstHoldsLock(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "an")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("AGENTNET_HOME", home)

	done := make(chan int, 1)
	go func() { done <- Main([]string{"daemon"}, "test") }()
	waitForSocket(t, paths.Socket())
	defer func() {
		client, err := ipc.Dial(paths.Socket())
		if err == nil {
			client.Call(context.Background(), "shutdown", nil, nil)
			client.Close()
		}
		<-done
	}()

	code := Main([]string{"daemon"}, "test")
	if code != 0 {
		t.Fatalf("second daemon exited %d, want 0", code)
	}
}
