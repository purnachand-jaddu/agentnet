package lan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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
	_, err = a.Send(context.Background(), bAddr, e)
	if err == nil {
		t.Fatal("Send of an oversized envelope should error")
	}
	if !strings.Contains(err.Error(), "413") {
		t.Fatalf("Send error = %q, want it to mention 413", err)
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

// addresses doesn't touch the network for a specific bind IP, so a documentation-range IP (never actually bound) is safe to use here.
func TestAddressesSpecificNonLoopbackBind(t *testing.T) {
	tr := New("203.0.113.5", 4321)
	addrs, err := tr.addresses(4321)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	want := []string{"lan://203.0.113.5:4321"}
	if len(addrs) != 1 || addrs[0] != want[0] {
		t.Fatalf("addrs = %v, want %v", addrs, want)
	}
}

// httpClient's Transport.Proxy must be nil. net/http's env-proxy lookup already exempts loopback/localhost
// targets (see net/http's useProxy), so a t.Setenv HTTP_PROXY test against our loopback-only test servers
// would pass even with the bug reintroduced; this direct field check is the one that actually catches it.
func TestHTTPClientHasNoProxy(t *testing.T) {
	tr, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport is %T, want *http.Transport", httpClient.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("httpClient's Transport.Proxy must be nil, so Send never honors HTTP_PROXY/HTTPS_PROXY")
	}
}

func TestSendIgnoresProxyEnv(t *testing.T) {
	// A proxy nothing listens on: if Send honoured it, the request would fail to connect instead of succeeding.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	_, bAddr := listenLoopback(t, func(ctx context.Context, e envelope.Envelope) envelope.Ack {
		return envelope.Ack{ID: e.ID, State: message.StateQueued}
	})
	a := New("127.0.0.1", 0)
	t.Cleanup(func() { a.Close() })
	if _, err := a.Listen(context.Background(), nil); err != nil {
		t.Fatalf("A.Listen: %v", err)
	}

	ack, err := a.Send(context.Background(), bAddr, testEnvelope("proxy-test"))
	if err != nil {
		t.Fatalf("Send: %v (Send must not honor HTTP_PROXY for lan:// peers)", err)
	}
	if ack.State != message.StateQueued {
		t.Fatalf("ack.State = %q, want %q", ack.State, message.StateQueued)
	}
}

func TestSendRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"explicit seconds", "7", 7 * time.Second},
		{"missing header", "", defaultRetryAfter},
		{"unparseable header", "soon", defaultRetryAfter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.header != "" {
					w.Header().Set("Retry-After", c.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()

			a := New("127.0.0.1", 0)
			t.Cleanup(func() { a.Close() })
			_, err := a.Send(context.Background(), "lan://"+strings.TrimPrefix(srv.URL, "http://"), testEnvelope("x"))

			var rae *RetryAfterError
			if !errors.As(err, &rae) {
				t.Fatalf("err = %v, want *RetryAfterError", err)
			}
			if rae.After != c.want {
				t.Fatalf("After = %v, want %v", rae.After, c.want)
			}
		})
	}
}

// Addrs re-reads the interfaces on each call, so a network change shows up without a re-Listen.
func TestAddrsFollowInterfaceChanges(t *testing.T) {
	tr := New("", 0)
	ips := []net.IP{net.ParseIP("192.0.2.10")}
	tr.IPs = func() ([]net.IP, error) { return ips, nil }
	if got, err := tr.Addrs(); err != nil || got != nil {
		t.Fatalf("before Listen: %v, %v; want none", got, err)
	}
	tr.bound = 4321 // stands in for Listen, which would bind every interface
	if got, _ := tr.Addrs(); !slices.Equal(got, []string{"lan://192.0.2.10:4321"}) {
		t.Fatalf("addrs = %v", got)
	}
	ips = []net.IP{net.ParseIP("198.51.100.7")}
	if got, _ := tr.Addrs(); !slices.Equal(got, []string{"lan://198.51.100.7:4321"}) {
		t.Fatalf("after change: addrs = %v", got)
	}
}
