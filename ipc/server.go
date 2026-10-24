package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// Listen binds the Unix socket at path, refusing if a daemon already answers there and removing a stale file otherwise.
func Listen(path string) (net.Listener, error) {
	if nc, err := net.Dial("unix", path); err == nil {
		nc.Close()
		return nil, fmt.Errorf("another daemon is listening on %s", path)
	}
	os.Remove(path) // stale socket from a dead daemon; fine if it's not there
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// conn is one accepted connection: a reader goroutine per request and a single writer goroutine, fed by out, so replies and pushed events never interleave mid-line.
type conn struct {
	id   uint64
	nc   net.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func newConn(id uint64, nc net.Conn) *conn {
	return &conn{id: id, nc: nc, out: make(chan []byte, 16), done: make(chan struct{})}
}

func (c *conn) ID() uint64 { return c.id }

// close is idempotent: it may run from the reader loop or the writer loop, whichever notices first.
func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

func (c *conn) Push(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	select {
	case c.out <- b:
		return nil
	case <-c.done:
		return errors.New("ipc: connection closed")
	}
}

func (c *conn) writeLine(b []byte) {
	select {
	case c.out <- b:
	case <-c.done:
	}
}

func (c *conn) writeLoop() {
	for {
		select {
		case b := <-c.out:
			if _, err := c.nc.Write(b); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

// Serve accepts connections on l until ctx is cancelled (which closes l) or Accept fails, running h for every request and calling onClose exactly once per connection.
func Serve(ctx context.Context, l net.Listener, h Handler, onClose func(Conn)) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var nextID atomic.Uint64
	for {
		nc, err := l.Accept()
		if err != nil {
			return err
		}
		c := newConn(nextID.Add(1), nc)
		go serveConn(ctx, c, h, onClose)
	}
}

func serveConn(ctx context.Context, c *conn, h Handler, onClose func(Conn)) {
	go c.writeLoop()
	defer func() {
		c.close()
		if onClose != nil {
			onClose(c)
		}
	}()

	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 4096), maxLine)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...) // Scanner reuses its buffer; the handler goroutine needs its own copy
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleLine(ctx, c, h, line)
		}()
	}
	wg.Wait()
}

func handleLine(ctx context.Context, c *conn, h Handler, line []byte) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		reply(c, Reply{ID: 0, OK: false, Error: err.Error()})
		return
	}
	result, err := h(ctx, c, req.Op, req.Args)
	if err != nil {
		reply(c, Reply{ID: req.ID, OK: false, Error: err.Error()})
		return
	}
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			reply(c, Reply{ID: req.ID, OK: false, Error: err.Error()})
			return
		}
		raw = b
	}
	reply(c, Reply{ID: req.ID, OK: true, Result: raw})
}

func reply(c *conn, r Reply) {
	b, err := json.Marshal(r)
	if err != nil {
		return // r is ours to build; a marshal failure here isn't recoverable by retrying
	}
	c.writeLine(append(b, '\n'))
}
