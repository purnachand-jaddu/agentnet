package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
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

// cmdDaemon runs `agentnet daemon [--service]`.
func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	service := fs.Bool("service", false, "wait for the lock instead of exiting (launchd/systemd)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		// Ensure only does real work for the socket dir when Home's own socket path is too long for sun_path.
		if len(filepath.Join(paths.Home(), "agentnet.sock")) > 100 {
			dir := filepath.Join(os.TempDir(), "agentnet-"+strconv.Itoa(os.Getuid()))
			fmt.Fprintf(stderr, "remove %s and retry\n", dir)
		}
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

	l, err := ipc.Listen(paths.Socket())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	logger := log.New(stderr, "", log.LstdFlags)
	d, err := newDaemon(logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		l.Close()
		return 1
	}

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
func newDaemon(logger *log.Logger) (*daemon.Daemon, error) {
	st, err := sqlite.Open(paths.DB())
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return nil, err
	}
	return daemon.New(daemon.Options{
		Person: cfg.Person, Machine: cfg.Machine, Store: st, Adapters: adapters(),
		FolderAllowed: folderAllowed(paths.Config()), Now: time.Now,
		Log: func(format string, args ...any) { logger.Printf(format, args...) },
	}), nil
}

// folderAllowed returns a FolderAllowed func that reloads cfgPath at most once every 2s, so `folder add` takes
// effect without a restart.
func folderAllowed(cfgPath string) func(string) bool {
	var mu sync.Mutex
	var cfg config.Config
	var loaded time.Time
	return func(dir string) bool {
		mu.Lock()
		if time.Since(loaded) > 2*time.Second {
			if c, err := config.Load(cfgPath); err == nil {
				cfg = c
			}
			loaded = time.Now()
		}
		c := cfg
		mu.Unlock()
		return c.Allowed(dir)
	}
}
