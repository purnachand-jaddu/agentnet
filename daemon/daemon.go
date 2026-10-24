// Package daemon is the per-machine agentnet core: session registry, naming, send/deliver and hooks.
package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store"
)

// touchEvery and forgetEvery pace the periodic jobs Run drives.
const (
	touchEvery   = 10 * time.Minute
	forgetEvery  = time.Hour
	forgetMinAge = 24 * time.Hour
	forgetAfter  = 3 * time.Hour // never run ForgetSessions within this long of startup
)

// Options configures a Daemon.
type Options struct {
	Person, Machine string
	Store           store.Store
	Adapters        *adapter.Registry
	FolderAllowed   func(dir string) bool
	Now             func() time.Time
	Log             func(format string, args ...any)
	Service         bool // true when this process was started as the background service (--service)
}

// Daemon is one running agentnet daemon: session registry plus (later tasks) send/deliver and hooks.
type Daemon struct {
	opts Options
	mu   sync.Mutex

	live     map[string]*live // keyed by Session.Key
	conns    map[uint64]ipc.Conn
	inFlight map[string]time.Time
	pairs    map[[2]string][]time.Time
	arrived  chan struct{}
	waiting  map[string]int  // keys with a long-polling "inbox" call in flight
	watching map[string]bool // keys with a turn-watch goroutine already running
	started  time.Time
	shutdown chan struct{}

	gitMu    sync.Mutex // guards gitCache; never held together with mu
	gitCache map[string]gitInfo
}

// New builds a Daemon from o.
func New(o Options) *Daemon {
	return &Daemon{
		opts:     o,
		live:     map[string]*live{},
		conns:    map[uint64]ipc.Conn{},
		inFlight: map[string]time.Time{},
		pairs:    map[[2]string][]time.Time{},
		arrived:  make(chan struct{}),
		waiting:  map[string]int{},
		watching: map[string]bool{},
		started:  o.Now(),
		shutdown: make(chan struct{}),
		gitCache: map[string]gitInfo{},
	}
}

// Shutdown is closed once the "shutdown" op is handled.
func (d *Daemon) Shutdown() <-chan struct{} { return d.shutdown }

// Run drives periodic maintenance (touching live sessions, forgetting old ones) until ctx is done. Tests don't call it.
func (d *Daemon) Run(ctx context.Context) {
	touch := time.NewTicker(touchEvery)
	defer touch.Stop()
	forget := time.NewTicker(forgetEvery)
	defer forget.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-touch.C:
			d.touchLive()
		case <-forget.C:
			if d.opts.Now().Sub(d.started) >= forgetAfter {
				d.forgetOld()
			}
		}
	}
}

// touchLive refreshes the remembered record's last-seen time for every live session.
func (d *Daemon) touchLive() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.opts.Now()
	for _, l := range d.live {
		rec := store.SessionRecord{Name: l.name, Key: l.s.Key, Agent: l.s.Agent, Cwd: l.s.Cwd, LastSeen: now}
		if err := d.opts.Store.UpsertSession(rec); err != nil {
			d.opts.Log("daemon: touch %s: %v", l.name, err)
		}
	}
}

// forgetOld prunes remembered sessions not seen in forgetMinAge.
func (d *Daemon) forgetOld() {
	n, err := d.opts.Store.ForgetSessions(d.opts.Now().Add(-forgetMinAge))
	if err != nil {
		d.opts.Log("daemon: forget sessions: %v", err)
		return
	}
	if n > 0 {
		d.opts.Log("daemon: forgot %d old sessions", n)
	}
}
