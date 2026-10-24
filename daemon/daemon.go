// Package daemon is the per-machine agentnet core: session registry, naming, send/deliver and hooks.
package daemon

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/envelope"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/seal"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/team"
	"github.com/purnachand-jaddu/agentnet/transport"
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
	Person, Machine  string
	Store            store.Store
	Adapters         *adapter.Registry
	FolderAllowed    func(dir string) bool
	Paused           func() bool                       // true refuses every incoming message; nil is never paused
	PolicyFor        func(person string) config.Policy // inbound policy for person; nil accepts everyone
	InvalidatePolicy func()                            // forces the next PolicyFor read to reload; nil is a no-op
	ConfigPath       string                            // config.json, for approve/refuse to edit via config.Update
	Auditor          Auditor                           // nil defaults to NoAudit
	Now              func() time.Time
	Log              func(format string, args ...any)
	Service          bool // true when this process was started as the background service (--service)
	Config           config.Config

	Team          *team.State // nil when not in a network: no networking at all
	Keys          identity.Keys
	Transports    []transport.Transport
	Discoverers   []discovery.Discoverer
	Sealer        seal.Sealer   // seal.HPKE if nil
	Window        time.Duration // accepted clock skew on envelopes; 5 min if zero
	PresenceEvery time.Duration // presence resend interval; 60 s if zero
	OfflineAfter  time.Duration // a machine silent this long is offline; 3 min if zero
	Debounce      time.Duration // how often local changes are checked and sent; 2 s if zero
	OutboxEvery   time.Duration // outbox retry interval; 30 s if zero
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

	verifier     envelope.Verifier
	peers        map[string]*peer           // remote member machines, keyed by peerKey; guarded by mu
	statics      []string                   // keyless discovery hints (static peers); guarded by mu
	myAddrs      []string                   // this machine's transport addresses; guarded by mu
	addrSrc      []func() ([]string, error) // each listening transport's current addresses; set by startNetwork, then gossip only
	stopAnnounce context.CancelFunc         // ends the current discovery announcement; startNetwork, then gossip only
	netDirty     bool                       // something changed that warrants sending presence now; guarded by mu
	addrSkip     map[string]time.Time       // endpoints that asked us to wait, until when; guarded by mu
	unsaved      atomic.Bool                // a merged team change hasn't reached the store yet
	netWG        sync.WaitGroup             // network goroutines Run waits for

	sendMu   map[string]*sync.Mutex // per-peer lock serialising message sends; guarded by mu
	flushing map[string]bool        // peers with a flusher running; guarded by mu
	dirty    map[string]time.Time   // peers with a flush requested, and the newest cutoff asked for; guarded by mu
	kick     chan struct{}          // wakes the outbox loop to start flushers for dirty peers
	rate     map[string][]time.Time // recent incoming message times per sender peerKey; guarded by mu
	admitMu  [64]sync.Mutex         // admission locks for incoming messages, sharded by id hash; taken before mu, never inside it

	policyMu sync.RWMutex // guards a policy change against admission still mid-flight under the old policy
}

// New builds a Daemon from o.
func New(o Options) *Daemon {
	if o.Sealer == nil {
		o.Sealer = seal.HPKE{}
	}
	if o.Auditor == nil {
		o.Auditor = NoAudit{}
	}
	o.Window = cmp.Or(o.Window, 5*time.Minute)
	o.PresenceEvery = cmp.Or(o.PresenceEvery, time.Minute)
	o.OfflineAfter = cmp.Or(o.OfflineAfter, 3*time.Minute)
	o.Debounce = cmp.Or(o.Debounce, 2*time.Second)
	o.OutboxEvery = cmp.Or(o.OutboxEvery, 30*time.Second)
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
		verifier: envelope.Verifier{Team: o.Team, Replay: envelope.NewReplay(2 * o.Window), Now: o.Now, Window: o.Window},
		peers:    map[string]*peer{},
		addrSkip: map[string]time.Time{},
		sendMu:   map[string]*sync.Mutex{},
		flushing: map[string]bool{},
		dirty:    map[string]time.Time{},
		kick:     make(chan struct{}, 1),
		rate:     map[string][]time.Time{},
	}
}

// Shutdown is closed once the "shutdown" op is handled.
func (d *Daemon) Shutdown() <-chan struct{} { return d.shutdown }

// Run drives periodic maintenance and, when in a network, presence gossip until ctx is done.
func (d *Daemon) Run(ctx context.Context) {
	if d.opts.Team != nil {
		d.startNetwork(ctx)
		defer d.stopNetwork()
	}
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
			d.pruneReceived()
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
