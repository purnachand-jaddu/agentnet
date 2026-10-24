package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

// badNameMsg is printed when --person/--machine fails message.Slug, before any approver is asked.
const badNameMsg = "person and machine names must use lowercase letters, digits and -"

// openStoreIfExists opens the sqlite store at paths.DB(), or returns (nil, nil) without creating a
// database file if one doesn't exist yet.
func openStoreIfExists() (store.Store, error) {
	if _, err := os.Stat(paths.DB()); err != nil {
		return nil, nil
	}
	return sqlite.Open(paths.DB())
}

// personMachine resolves --person/--machine flags against config defaults.
func personMachine(person, machine *string) (p, m string, err error) {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return "", "", err
	}
	p, m = *person, *machine
	if p == "" {
		p = cfg.Person
	}
	if m == "" {
		m = cfg.Machine
	}
	return p, m, nil
}

// saveConfigPersonMachine updates and saves person/machine to config.json; a var so tests can force a failure.
var saveConfigPersonMachine = func(person, machine string) error {
	return config.Update(paths.Config(), func(c *config.Config) error {
		c.Person, c.Machine = person, machine
		return nil
	})
}

// loadOrGenerateMachineKeys loads this machine's keys, generating and saving a fresh pair if none exist yet.
func loadOrGenerateMachineKeys() (identity.Keys, error) {
	p := paths.MachineKey()
	if _, err := os.Stat(p); err == nil {
		return identity.LoadKeys(p)
	} else if !os.IsNotExist(err) {
		return identity.Keys{}, err
	}
	keys, err := identity.GenerateKeys()
	if err != nil {
		return identity.Keys{}, err
	}
	if err := keys.Save(p); err != nil {
		return identity.Keys{}, err
	}
	return keys, nil
}

// saveFounderKeyOnce writes priv's seed to path, 0600, refusing to overwrite an existing file. O_EXCL makes
// the create-and-check one atomic step (no link/rename trick needed, so this also works on exFAT/FAT media).
// A write or sync failure removes the partial file rather than leaving it behind.
func saveFounderKeyOnce(path string, priv ed25519.PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return errors.New("a network already exists here")
		}
		return err
	}
	if _, err := f.Write([]byte(base64.RawURLEncoding.EncodeToString(priv.Seed()))); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// loadFounderKey reads a founder key written by saveFounderKeyOnce, refusing one whose mode allows group or other access.
func loadFounderKey(path string) (ed25519.PrivateKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %03o allows group or other access, chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a valid founder key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// cmdInit runs `agentnet init [--person P] [--machine M] [--founder-key path]`.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	person := fs.String("person", "", "the person's name")
	machine := fs.String("machine", "", "this machine's name")
	founderKeyFlag := fs.String("founder-key", "", "where to write the founder key")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, m, err := personMachine(person, machine)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !message.Slug(p) || !message.Slug(m) {
		fmt.Fprintln(stderr, badNameMsg)
		return 1
	}

	existingStore, err := openStoreIfExists()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if existingStore != nil {
		existing, err := team.Load(existingStore)
		existingStore.Close()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if existing != nil {
			fmt.Fprintf(stderr, "already in network %s\n", identity.TeamCode(existing.Founder))
			return 1
		}
	}

	ok, err := newApprover().Confirm(context.Background(),
		fmt.Sprintf("Create a new agentnet network on this computer as %s@%s?", p, m), "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	founderKeyPath := paths.FounderKey()
	if *founderKeyFlag != "" {
		founderKeyPath = *founderKeyFlag
	}
	_, founderPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := saveFounderKeyOnce(founderKeyPath, founderPriv); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(founderKeyPath) // this init failed after writing the founder key: don't leave it behind
		}
	}()

	keys, err := loadOrGenerateMachineKeys()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	dbStore, err := sqlite.Open(paths.DB())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer dbStore.Close()

	if _, err := team.Create(dbStore, keys, founderPriv, p, m, time.Now()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// The network now exists in the store: keep the founder key regardless of what happens below.
	// person/machine already live in the self cert, so a config-save failure here is a warning, not fatal —
	// stranding a network with no founder key (because cleanup ran after Create had already succeeded) is worse.
	committed = true

	if err := saveConfigPersonMachine(p, m); err != nil {
		fmt.Fprintf(stderr, "warning: could not save person/machine to config: %v\n", err)
	}

	restartDaemon()

	fmt.Fprintln(stdout, identity.TeamCode(founderPriv.Public().(ed25519.PublicKey)))
	fmt.Fprintf(stdout, "Add people with `agentnet admit <request>`. The founder key is at %s; keep it safe (moving it offline with --founder-key is recommended).\n", founderKeyPath)
	return 0
}

// cmdJoin runs `agentnet join [--person P] [--machine M]`.
func cmdJoin(args []string) int {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	person := fs.String("person", "", "the person's name")
	machine := fs.String("machine", "", "this machine's name")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p, m, err := personMachine(person, machine)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !message.Slug(p) || !message.Slug(m) {
		fmt.Fprintln(stderr, badNameMsg)
		return 1
	}

	ok, err := newApprover().Confirm(context.Background(),
		fmt.Sprintf("Ask to join an agentnet network as %s@%s?", p, m), "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	keys, err := loadOrGenerateMachineKeys()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req := team.NewRequest(keys, p, m)

	st, err := sqlite.Open(paths.DB())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	if err := st.Put("team", "pending", []byte(req.Encode())); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := saveConfigPersonMachine(p, m); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintln(stdout, req.Encode())
	fmt.Fprintf(stdout, "Send this request to whoever runs the network. They'll read the pairing code back to you; check it matches: %s. They'll also tell you their team code.\n", req.PairingCode())
	return 0
}

// cmdAdmit runs `agentnet admit <request> [--founder-key path]`.
func cmdAdmit(args []string) int {
	fs := flag.NewFlagSet("admit", flag.ContinueOnError)
	founderKeyFlag := fs.String("founder-key", "", "where the founder key lives")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agentnet admit <request> [--founder-key path]")
		return 2
	}

	founderKeyPath := paths.FounderKey()
	if *founderKeyFlag != "" {
		founderKeyPath = *founderKeyFlag
	}
	founderPriv, err := loadFounderKey(founderKeyPath)
	if err != nil {
		fmt.Fprintf(stderr, "only the founder can admit people: %v\n", err)
		return 1
	}

	req, err := identity.DecodeJoinRequest(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	approver := newApprover()
	detail := fmt.Sprintf("Person: %s\nMachine: %s\nPairing code: %s\n\nAdmit only if the joiner reads you this exact pairing code.",
		req.Person, req.Machine, req.PairingCode())
	ok, err := approver.Confirm(context.Background(), "Admit this person?", detail)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	st, err := sqlite.Open(paths.DB())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()

	bundle, err := team.Admit(st, founderPriv, req, false, time.Now())
	if errors.Is(err, team.ErrPersonTaken) {
		ok2, err2 := approver.Confirm(context.Background(),
			fmt.Sprintf("%s is already a member on another computer. Is this the same person?", req.Person), "")
		if err2 != nil {
			fmt.Fprintln(stderr, err2)
			return 1
		}
		if !ok2 {
			fmt.Fprintln(stderr, "not approved")
			return 1
		}
		bundle, err = team.Admit(st, founderPriv, req, true, time.Now())
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintln(stdout, bundle.Encode())
	fmt.Fprintf(stdout, "Tell them your team code: %s\n", identity.TeamCode(bundle.Founder))
	return 0
}

// cmdAccept runs `agentnet accept <bundle>`.
func cmdAccept(args []string) int {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agentnet accept <bundle>")
		return 2
	}

	b, err := identity.DecodeBundle(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	if _, err := os.Stat(paths.MachineKey()); err != nil {
		fmt.Fprintln(stderr, "run `agentnet join` on this machine first")
		return 1
	}

	detail := fmt.Sprintf("Team code: %s\n\nAccept only if this matches the team code the founder told you.", identity.TeamCode(b.Founder))
	ok, err := newApprover().Confirm(context.Background(), "Accept this network?", detail)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	keys, err := identity.LoadKeys(paths.MachineKey())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	st, err := sqlite.Open(paths.DB())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()

	if _, err := team.Accept(st, keys, b, time.Now()); err != nil {
		if errors.Is(err, team.ErrOtherNetwork) {
			fmt.Fprintf(stderr, "already in another network; leave first (remove %s)\n", paths.Home())
			return 1
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := st.Delete("team", "pending"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	restartDaemon()

	fmt.Fprintf(stdout, "Joined network %s as %s@%s.\n", identity.TeamCode(b.Founder), b.Cert.Person, b.Cert.Machine)
	return 0
}

// restartDaemon tells a running daemon to shut down (it's meant to come back up with the new team); a daemon
// that isn't running is not an error.
func restartDaemon() {
	c, err := ipc.Dial(paths.Socket())
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.Call(context.Background(), "shutdown", nil, nil)
}
