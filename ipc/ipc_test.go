package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCallAndPush(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan uint64, 1)
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		if op != "echo" {
			return nil, errors.New("unknown op")
		}
		var s string
		json.Unmarshal(args, &s)
		go c.Push(Event{Event: "deliver", Key: "k", Text: s})
		return s, nil
	}, func(c Conn) { closed <- c.ID() })

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := c.Call(ctx, "echo", "hi", &got); err != nil || got != "hi" {
		t.Fatalf("Call = %q, %v", got, err)
	}
	select {
	case ev := <-c.Events():
		if ev.Text != "hi" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	if err := c.Call(ctx, "nope", nil, nil); err == nil || err.Error() != "unknown op" {
		t.Fatalf("error not relayed: %v", err)
	}
	c.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose not called")
	}
}

// 20 concurrent Calls over one Client must each get their own, correct reply.
func TestConcurrentCalls(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		var n int
		if err := json.Unmarshal(args, &n); err != nil {
			return nil, err
		}
		return n * 2, nil
	}, nil)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var got int
			if err := c.Call(ctx, "double", i, &got); err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if got != i*2 {
				errs <- fmt.Errorf("call %d: got %d, want %d", i, got, i*2)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Listen refuses when a daemon is already listening on the socket.
func TestListenRefusesLiveListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := Listen(sock); err == nil {
		t.Fatal("expected an error for a live listener")
	}
}

// Listen replaces a stale socket file left behind by a dead daemon.
func TestListenReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	l.Close() // nothing is listening now, but the file is still there

	l2, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen did not replace the stale socket: %v", err)
	}
	l2.Close()
}

// Pending Calls must error, and Events() must close, when the server side disappears.
func TestCallErrsWhenServerDies(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		nc, err := l.Accept()
		if err != nil {
			return
		}
		close(accepted)
		time.Sleep(50 * time.Millisecond)
		nc.Close() // drop the connection without ever replying
	}()

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	<-accepted

	errCh := make(chan error, 1)
	go func() { errCh <- c.Call(context.Background(), "op", nil, nil) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error once the server dropped the connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the server went away")
	}

	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("expected Events() to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events() was not closed")
	}
}
