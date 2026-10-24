package cli

import (
	"errors"
	"io"
	"log"
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

func TestRunOrRetryServiceModeRetriesUntilSuccess(t *testing.T) {
	oldInitial, oldMax := retryInitialDelay, retryMaxDelay
	retryInitialDelay, retryMaxDelay = 2*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { retryInitialDelay, retryMaxDelay = oldInitial, oldMax })

	logger := log.New(io.Discard, "", 0)
	attempts := 0
	err := runOrRetry(true, logger, "test step", func() error {
		attempts++
		if attempts < 3 {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("runOrRetry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRunOrRetryNonServiceModeFailsOnce(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	attempts := 0
	err := runOrRetry(false, logger, "test step", func() error {
		attempts++
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry without --service)", attempts)
	}
}
