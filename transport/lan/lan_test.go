package lan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/transport"
)

func testEnvelope(id string) envelope.Envelope {
	return envelope.Envelope{V: 1, Kind: "msg", ID: id, TS: 1, To: []byte("to"), Sealed: []byte("sealed-bytes")}
}

func listenLoopback(t *testing.T, h transport.Handler) (*Transport, string) {
	t.Helper()
	tr := New("127.0.0.1", 0)
	t.Cleanup(func() { tr.Close() })
	addrs, err := tr.Listen(context.Background(), h)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if len(addrs) != 1 {
		t.Fatalf("got %d addrs, want 1: %v", len(addrs), addrs)
	}
	return tr, addrs[0]
}

func TestSendAndAck(t *testing.T) {
	var got envelope.Envelope
	b, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		got = e
		return envelope.Ack{ID: e.ID, State: message.StateQueued}
	})
	_ = b

	a := New("127.0.0.1", 0)
	t.Cleanup(func() { a.Close() })
	if _, err := a.Listen(context.Background(), func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		t.Fatal("A's handler should not be called")
		return envelope.Ack{}
	}); err != nil {
		t.Fatalf("A.Listen: %v", err)
	}

	e := testEnvelope("msg-1")
	ack, err := a.Send(context.Background(), bAddr, e)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.State != message.StateQueued {
		t.Fatalf("ack.State = %q, want %q", ack.State, message.StateQueued)
	}
	if ack.ID != e.ID {
		t.Fatalf("ack.ID = %q, want %q", ack.ID, e.ID)
	}
	if got.ID != e.ID {
		t.Fatalf("handler saw ID %q, want %q", got.ID, e.ID)
	}
}

func TestSendToClosedPort(t *testing.T) {
	// Bind, learn the port, then close it: nobody is listening there any more.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("lan://%s", ln.Addr().String())
	ln.Close()

	a := New("127.0.0.1", 0)
	t.Cleanup(func() { a.Close() })

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := a.Send(context.Background(), addr, testEnvelope("x"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send to a closed port should error")
		}
		if time.Since(start) > 6*time.Second {
			t.Fatalf("Send took %v, want < 6s", time.Since(start))
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Send did not return within 6s")
	}
}

func TestBodyTooLarge(t *testing.T) {
	_, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		t.Fatal("handler should not run for an oversized body")
		return envelope.Ack{}
	})

	// A 2 MiB body, valid JSON on a known field, so the decoder reads into the oversized value instead of failing on syntax or an unknown field first.
	host := strings.TrimPrefix(bAddr, "lan://")
	body := append(append([]byte(`{"id":"`), bytes.Repeat([]byte("a"), 2<<20)...), []byte(`"}`)...)
	resp, err := http.Post("http://"+host+"/v1/envelope", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}

	a := New("127.0.0.1", 0)
	t.Cleanup(func() { a.Close() })
	if _, err := a.Listen(context.Background(), nil); err != nil {
		t.Fatalf("A.Listen: %v", err)
	}
	e := testEnvelope("big")
	e.Sealed = bytes.Repeat([]byte("a"), 2<<20)
	if _, err := a.Send(context.Background(), bAddr, e); err == nil {
		t.Fatal("Send of an oversized envelope should error")
	}
}

func TestRateLimit(t *testing.T) {
	var calls atomic.Int64
	_, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		calls.Add(1)
		return envelope.Ack{ID: e.ID, State: message.StateQueued}
	})
	host := strings.TrimPrefix(bAddr, "lan://")

	var got429 int
	for i := 0; i < 25; i++ {
		body, _ := json.Marshal(testEnvelope(fmt.Sprintf("r-%d", i)))
		resp, err := http.Post("http://"+host+"/v1/envelope", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("http.Post #%d: %v", i, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			got429++
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 response missing Retry-After header")
			}
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got429 == 0 {
		t.Fatal("25 rapid requests produced no 429s")
	}
}

func TestSendNonLanScheme(t *testing.T) {
	a := New("127.0.0.1", 0)
	t.Cleanup(func() { a.Close() })
	if _, err := a.Send(context.Background(), "http://127.0.0.1:9999/v1/envelope", testEnvelope("x")); err == nil {
		t.Fatal("Send with a non-lan scheme should error")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		return envelope.Ack{}
	})
	host := strings.TrimPrefix(bAddr, "lan://")
	resp, err := http.Get("http://" + host + "/v1/envelope")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestUnknownJSONField(t *testing.T) {
	_, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		t.Fatal("handler should not run for bad JSON")
		return envelope.Ack{}
	})
	host := strings.TrimPrefix(bAddr, "lan://")
	resp, err := http.Post("http://"+host+"/v1/envelope", "application/json", strings.NewReader(`{"v":1,"kind":"msg","id":"x","bogus":true}`))
	if err != nil {
		t.Fatalf("http.Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListenReturnsBoundPort(t *testing.T) {
	tr := New("127.0.0.1", 0)
	t.Cleanup(func() { tr.Close() })
	addrs, err := tr.Listen(context.Background(), func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		return envelope.Ack{}
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if len(addrs) != 1 || strings.HasSuffix(addrs[0], ":0") {
		t.Fatalf("addrs = %v, want a single address with a real port", addrs)
	}
}
