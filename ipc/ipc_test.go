package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// Regression for a send-on-closed-channel panic: Close() must not close
// Events() itself while readLoop's deliver() may still be sending to it.
func TestCloseDuringEventBurst(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		go func() {
			for i := 0; i < 300; i++ {
				c.Push(Event{Event: "deliver", Text: "x"})
			}
		}()
		return nil, nil
	}, nil)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "burst", nil, nil); err != nil {
		t.Fatal(err)
	}

	drained := make(chan struct{})
	go func() {
		for range c.Events() {
		}
		close(drained)
	}()
	c.Close()
	<-drained
}

// A handler long-polling on ctx must unblock, and onClose must fire, soon after the client disconnects.
func TestHandlerUnblocksOnDisconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan uint64, 1)
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		<-ctx.Done() // simulates a long-poll (e.g. inbox wait) that only returns when the client goes away
		return nil, ctx.Err()
	}, func(c Conn) { closed <- c.ID() })

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	go c.Call(context.Background(), "longpoll", nil, nil)
	time.Sleep(50 * time.Millisecond) // let the handler start blocking on ctx.Done()
	c.Close()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("onClose did not fire within 1s of the client closing")
	}
}

// A handler panic must become an error reply, not a crashed connection.
func TestHandlerPanicBecomesErrorReply(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		panic("boom")
	}, nil)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Call(context.Background(), "op", nil, nil)
	if err == nil || err.Error() != "internal error: boom" {
		t.Fatalf("Call = %v, want %q", err, "internal error: boom")
	}
}

// A malformed line gets an error reply with id 0 instead of dropping the connection.
func TestMalformedLineGetsIDZeroError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func(ctx context.Context, c Conn, op string, args json.RawMessage) (any, error) {
		return nil, nil
	}, nil)

	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := nc.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(nc)
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	var r Reply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != 0 || r.OK || r.Error == "" {
		t.Fatalf("reply = %+v, want id 0 and an error", r)
	}
}

// Events() drops the oldest queued event rather than blocking once its 64-slot buffer is full.
func TestEventsDropOldestAt64(t *testing.T) {
	c := &Client{events: make(chan Event, 64)}
	for i := 1; i <= 70; i++ {
		c.deliver(Event{Text: fmt.Sprintf("%d", i)})
	}
	if n := len(c.events); n != 64 {
		t.Fatalf("buffered = %d, want 64", n)
	}
	for want := 7; want <= 70; want++ {
		ev := <-c.events
		if ev.Text != fmt.Sprintf("%d", want) {
			t.Fatalf("got %q, want %q", ev.Text, fmt.Sprintf("%d", want))
		}
	}
}
