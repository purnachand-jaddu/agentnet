package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
)

// wireMsg decodes a line that is either a Reply or an Event; a non-empty Event field means it's an Event.
type wireMsg struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Key    string          `json:"key,omitempty"`
	IDs    []string        `json:"ids,omitempty"`
	Text   string          `json:"text,omitempty"`
}

// Client is one connection to the daemon.
type Client struct {
	nc         net.Conn
	writeMu    sync.Mutex // serializes writes so concurrent Calls don't interleave mid-line
	mu         sync.Mutex
	pending    map[uint64]chan Reply
	nextID     atomic.Uint64
	events     chan Event
	closed     chan struct{}
	closedOnce sync.Once // guards close(c.closed), which both Close and readLoop may reach
}

// Dial connects to the daemon's socket and starts routing replies and pushed events.
func Dial(path string) (*Client, error) {
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{
		nc:      nc,
		pending: make(map[uint64]chan Reply),
		events:  make(chan Event, 64),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Call marshals args (nil is omitted), sends the request, and waits for the matching reply or ctx.Done.
// result may be nil to ignore the result. An error reply surfaces as errors.New(reply.Error).
func (c *Client) Call(ctx context.Context, op string, args, result any) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}

	id := c.nextID.Add(1)
	ch := make(chan Reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	b, err := json.Marshal(Request{ID: id, Op: op, Args: raw})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	_, err = c.nc.Write(b)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case r := <-ch:
		if !r.OK {
			return errors.New(r.Error)
		}
		if result != nil && r.Result != nil {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("ipc: connection closed")
	}
}

// Events delivers pushed Events; when full (64), the oldest is dropped rather than blocking the reader. Closed once the connection dies.
func (c *Client) Events() <-chan Event { return c.events }

// Close tears down the connection; pending Calls then error and Events() closes.
func (c *Client) Close() error {
	err := c.nc.Close()
	c.closedOnce.Do(func() { close(c.closed) })
	return err
}

func (c *Client) readLoop() {
	// Only this goroutine ever sends on c.events (via deliver), so only it may close it —
	// closing from Close() too would race a concurrent deliver() and panic.
	defer func() {
		c.closedOnce.Do(func() { close(c.closed) })
		close(c.events)
	}()
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 4096), maxLine)
	for sc.Scan() {
		var msg wireMsg
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue // a malformed line from the daemon; nothing to route it to
		}
		if msg.Event != "" {
			c.deliver(Event{Event: msg.Event, Key: msg.Key, IDs: msg.IDs, Text: msg.Text})
			continue
		}
		c.mu.Lock()
		ch := c.pending[msg.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- Reply{ID: msg.ID, OK: msg.OK, Result: msg.Result, Error: msg.Error}
		}
	}
}

// deliver pushes ev, dropping the oldest queued event instead of blocking when Events() is full.
func (c *Client) deliver(ev Event) {
	select {
	case c.events <- ev:
	default:
		select {
		case <-c.events:
		default:
		}
		select {
		case c.events <- ev:
		default:
		}
	}
}
