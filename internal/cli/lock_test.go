package cli

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireLockNonBlockingSecondFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.lock")
	f1, err := acquireLock(path, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer f1.Close()

	if _, err := acquireLock(path, false); !errors.Is(err, errLocked) {
		t.Fatalf("second acquire: got %v, want errLocked", err)
	}
}

func TestAcquireLockWaitBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.lock")
	f1, err := acquireLock(path, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		f2, err := acquireLock(path, true)
		if err == nil {
			f2.Close()
		}
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("waiting acquire returned before the lock was released")
	case <-time.After(200 * time.Millisecond):
	}

	f1.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting acquire: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting acquire never returned after release")
	}
}
