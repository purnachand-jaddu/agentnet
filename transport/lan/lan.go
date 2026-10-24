// Package lan sends and receives envelopes over plain HTTP on the local network.
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
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/transport"
)

// maxEnvelopeBytes caps the request body: a 1 MiB sealed payload, base64-encoded in JSON, plus 64 KiB for the rest of the envelope.
const maxEnvelopeBytes = 4*((1<<20+2)/3) + 64<<10

// maxAckBytes caps the Ack response body Send will read.
const maxAckBytes = 64 << 10

// httpClient ignores HTTP_PROXY/HTTPS_PROXY: a LAN envelope must go straight to the peer, never through an env-configured proxy.
var httpClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	},
}

// Transport is a transport.Transport that moves envelopes over HTTP between machines on the same network.
type Transport struct {
	bind string
	port int

	mu        sync.Mutex
	srv       *http.Server
	stopPrune chan struct{}
	closed    chan struct{} // closed by Close, so the ctx-watcher goroutine exits even if ctx never fires
	closeOnce sync.Once
	closeErr  error
}

// New makes a LAN transport that binds bind:port on Listen (port 0 picks a free port).
func New(bind string, port int) *Transport {
	if bind == "" {
		bind = "0.0.0.0"
	}
	return &Transport{bind: bind, port: port}
}

var _ transport.Transport = (*Transport)(nil)

// Schemes reports the address scheme this transport dials.
func (t *Transport) Schemes() []string { return []string{"lan"} }

// Listen starts the HTTP server and returns this machine's lan:// addresses; it serves until ctx is done or Close.
func (t *Transport) Listen(ctx context.Context, h transport.Handler) ([]string, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(t.bind, strconv.Itoa(t.port)))
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	rl := newLimiter()
	stopPrune := make(chan struct{})
	go rl.pruneLoop(stopPrune)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/envelope", envelopeHandler(h, rl))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	t.mu.Lock()
	t.srv = srv
	t.stopPrune = stopPrune
	t.closed = make(chan struct{})
	closed := t.closed
	t.mu.Unlock()

	go srv.Serve(ln) // returns http.ErrServerClosed on Close/Shutdown; nothing to report
	go func() {
		select {
		case <-ctx.Done():
			t.Close()
		case <-closed: // Close was called directly; nothing left to watch for
		}
	}()

	addrs, err := t.addresses(port)
	if err != nil {
		t.Close()
		return nil, err
	}
	return addrs, nil
}

// addresses lists this machine's lan:// addresses for the bound port: loopback if bind is loopback, the bind IP alone if it's a specific non-loopback address, else one per up, non-loopback IPv4 interface address.
func (t *Transport) addresses(port int) ([]string, error) {
	if ip := net.ParseIP(t.bind); ip != nil && ip.IsLoopback() {
		return []string{fmt.Sprintf("lan://127.0.0.1:%d", port)}, nil
	}
	if ip := net.ParseIP(t.bind); ip != nil && !ip.IsUnspecified() {
		return []string{fmt.Sprintf("lan://%s:%d", ip.String(), port)}, nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue // ponytail: skip interfaces we can't enumerate rather than fail Listen entirely
		}
		for _, a := range ifaceAddrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			addrs = append(addrs, fmt.Sprintf("lan://%s:%d", ip4.String(), port))
		}
	}
	return addrs, nil
}

// envelopeHandler decodes one envelope, calls h, and writes the Ack as JSON.
func envelopeHandler(h transport.Handler, rl *limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip, time.Now()) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxEnvelopeBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var e envelope.Envelope
		if err := dec.Decode(&e); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		ack := h(r.Context(), e)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ack)
	}
}

// clientIP is the request's source IP, without its port, for rate limiting.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RetryAfterError is returned by Send when the peer answered 429; After is how long it asked the caller to wait.
type RetryAfterError struct {
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("lan: rate limited, retry after %s", e.After)
}

// defaultRetryAfter is used when a 429 response has no Retry-After header, or an unparseable one.
const defaultRetryAfter = 10 * time.Second

// parseRetryAfter reads a Retry-After header's seconds form, falling back to defaultRetryAfter.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return defaultRetryAfter
	}
	return time.Duration(secs) * time.Second
}

// Send posts e to addr's /v1/envelope and returns its signed Ack.
func (t *Transport) Send(ctx context.Context, addr string, e envelope.Envelope) (envelope.Ack, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return envelope.Ack{}, fmt.Errorf("lan: bad address %q: %w", addr, err)
	}
	if u.Scheme != "lan" {
		return envelope.Ack{}, fmt.Errorf("lan: unsupported scheme %q", u.Scheme)
	}

	body, err := json.Marshal(e)
	if err != nil {
		return envelope.Ack{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+u.Host+"/v1/envelope", bytes.NewReader(body))
	if err != nil {
		return envelope.Ack{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return envelope.Ack{}, fmt.Errorf("lan: send to %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return envelope.Ack{}, &RetryAfterError{After: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode != http.StatusOK {
		return envelope.Ack{}, fmt.Errorf("lan: %s returned %s", addr, resp.Status)
	}

	var ack envelope.Ack
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAckBytes)).Decode(&ack); err != nil {
		return envelope.Ack{}, fmt.Errorf("lan: decode ack: %w", err)
	}
	return ack, nil
}

// Close shuts the server down gracefully, waiting up to 2s for in-flight requests.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		srv, stopPrune, closed := t.srv, t.stopPrune, t.closed
		t.mu.Unlock()
		if closed != nil {
			close(closed)
		}
		if stopPrune != nil {
			close(stopPrune)
		}
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			t.closeErr = srv.Shutdown(ctx)
		}
	})
	return t.closeErr
}

// rateCapacity and rateRefill define the per-IP token bucket: 20 requests per 10s, refilled continuously.
const (
	rateCapacity = 20.0
	rateWindow   = 10 * time.Second
	rateRefill   = rateCapacity / float64(rateWindow/time.Second) // tokens per second
	bucketIdle   = time.Minute                                    // prune buckets untouched this long
)

// bucket is one source IP's token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter rate-limits requests per source IP.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}}
}

// allow reports whether ip may make a request now, consuming one token if so.
func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{tokens: rateCapacity, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(rateCapacity, b.tokens+now.Sub(b.last).Seconds()*rateRefill)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets idle longer than bucketIdle, so memory stays bounded.
func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if now.Sub(b.last) > bucketIdle {
			delete(l.buckets, ip)
		}
	}
}

func (l *limiter) pruneLoop(stop <-chan struct{}) {
	t := time.NewTicker(bucketIdle)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			l.prune(now)
		case <-stop:
			return
		}
	}
}
