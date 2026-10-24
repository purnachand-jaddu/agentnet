package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/adapter/claude"
	"github.com/purnachand-jaddu/agentnet/adapter/codex"
	"github.com/purnachand-jaddu/agentnet/adapter/generic"
	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/discovery"
	"github.com/purnachand-jaddu/agentnet/discovery/mdns"
	"github.com/purnachand-jaddu/agentnet/discovery/static"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
	"github.com/purnachand-jaddu/agentnet/transport"
	"github.com/purnachand-jaddu/agentnet/transport/lan"
)

// adapters is the registry shared by the daemon and the MCP server, so they agree on names.
func adapters() *adapter.Registry {
	return adapter.NewRegistry(claude.New(), codex.New(nil), generic.New(), generic.CLI())
}

// errLocked means acquireLock(path, false) found the lock already held.
var errLocked = errors.New("already locked")

// acquireLock takes the exclusive flock on path; wait blocks until it's free instead of reporting errLocked.
func acquireLock(path string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if !wait {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

// retryInitialDelay and retryMaxDelay bound --service's startup retry backoff; tests shrink them.
var (
	retryInitialDelay = time.Second
	retryMaxDelay     = 30 * time.Second
)

// runOrRetry calls fn once, or with service set retries it with backoff (retryInitialDelay to retryMaxDelay)
// until it succeeds: a launchd agent that exits quickly can lose local network access (Apple TN3179).
func runOrRetry(service bool, logger *log.Logger, label string, fn func() error) error {
	if !service {
		return fn()
	}
	delay := retryInitialDelay
	for {
		if err := fn(); err == nil {
			return nil
		} else {
			logger.Printf("daemon: %s: %v; retrying in %s", label, err, delay)
		}
		time.Sleep(delay)
		delay *= 2
		if delay > retryMaxDelay {
			delay = retryMaxDelay
		}
	}
}

// ensureHome runs paths.Ensure(), adding a socket-dir removal hint when that's the actual cause.
func ensureHome() error {
	if err := paths.Ensure(); err != nil {
		// Ensure only does real work for the socket dir when Home's own socket path is too long for sun_path.
		if len(filepath.Join(paths.Home(), "agentnet.sock")) > 100 {
			dir := filepath.Join(os.TempDir(), "agentnet-"+strconv.Itoa(os.Getuid()))
			return fmt.Errorf("%w; remove %s and retry", err, dir)
		}
		return err
	}
	return nil
}

// cmdDaemon runs `agentnet daemon [--service]`.
func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	service := fs.Bool("service", false, "wait for the lock instead of exiting (launchd/systemd)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "", log.LstdFlags)

	if err := runOrRetry(*service, logger, "paths.Ensure", ensureHome); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	lockFile, err := acquireLock(paths.Lock(), *service)
	if err != nil {
		if errors.Is(err, errLocked) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer lockFile.Close()

	var l net.Listener
	if err := runOrRetry(*service, logger, "ipc.Listen", func() error {
		ll, e := ipc.Listen(paths.Socket())
		if e == nil {
			l = ll
		}
		return e
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var d *daemon.Daemon
	var st io.Closer
	if err := runOrRetry(*service, logger, "newDaemon", func() error {
		dd, s, e := newDaemon(logger, *service)
		if e == nil {
			d, st = dd, s
		}
		return e
	}); err != nil {
		fmt.Fprintln(stderr, err)
		l.Close()
		return 1
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ipc.Serve(ctx, l, d.Handle, d.ConnClosed) }()
	go d.Run(ctx)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
	case <-d.Shutdown():
	}
	cancel()
	<-served
	return 0
}

// newDaemon builds the Daemon this process serves, reading its identity and folder opt-in from config.json.
// It also returns the sqlite store so the caller can close it on shutdown.
func newDaemon(logger *log.Logger, service bool) (*daemon.Daemon, io.Closer, error) {
	st, err := sqlite.Open(paths.DB())
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(paths.Config())
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	current, invalidate := configReloader(paths.Config(), 2*time.Second)
	logFn := func(format string, args ...any) { logger.Printf(format, args...) }
	o := daemon.Options{
		Person: cfg.Person, Machine: cfg.Machine, Store: st, Adapters: adapters(), Config: cfg,
		FolderAllowed:    func(dir string) bool { return current().Allowed(dir) },
		Paused:           func() bool { return current().Paused },
		PolicyFor:        func(person string) config.Policy { return current().PolicyFor(person, cfg.Person) },
		InvalidatePolicy: invalidate,
		ConfigPath:       paths.Config(),
		Now:              time.Now, Service: service,
		Log: logFn,
	}
	closer := closers{st}
	if au, err := daemon.NewFileAuditor(paths.Audit(), logFn); err != nil {
		logger.Printf("daemon: audit log unavailable: %v", err)
	} else {
		o.Auditor = au
		closer = append(closer, au)
	}
	if err := withNetwork(&o, cfg, st, logger); err != nil {
		closer.Close()
		return nil, nil, err
	}
	return daemon.New(o), closer, nil
}

// closers closes every one of them, continuing past an error and returning the first one.
type closers []io.Closer

func (cs closers) Close() error {
	var first error
	for _, c := range cs {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// withNetwork fills o's networking fields when this machine is in a network; without keys it stays local and logs why.
func withNetwork(o *daemon.Options, cfg config.Config, st *sqlite.Store, logger *log.Logger) error {
	t, err := team.Load(st)
	if err != nil || t == nil {
		return err
	}
	keys, err := identity.LoadKeys(paths.MachineKey())
	if err != nil {
		logger.Printf("daemon: in a network but machine key unreadable (%v); local only", err)
		return nil
	}
	o.Team, o.Keys = t, keys
	o.Transports = []transport.Transport{lan.New("", cfg.Port)}
	o.Discoverers = []discovery.Discoverer{&mdns.Discoverer{Self: keys.Public()}, static.New(cfg.Peers)}
	return nil
}

// configReloader returns a func giving cfgPath's config, reloaded at most once per every, plus an
// invalidate func that forces its next call to reload immediately (used after approve/refuse).
func configReloader(cfgPath string, every time.Duration) (get func() config.Config, invalidate func()) {
	var mu sync.Mutex
	var cfg config.Config
	var loaded time.Time
	get = func() config.Config {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(loaded) > every {
			if c, err := config.Load(cfgPath); err == nil {
				cfg = c
			}
			loaded = time.Now()
		}
		return cfg
	}
	invalidate = func() {
		mu.Lock()
		defer mu.Unlock()
		loaded = time.Time{}
	}
	return get, invalidate
}
