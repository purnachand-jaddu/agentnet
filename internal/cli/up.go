package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/team"
)

// cmdUp runs `agentnet up [--person P]`: the single command to get going.
func cmdUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	person := fs.String("person", "", "the person's name")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	t, pending, err := upStatus()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if t != nil {
		fmt.Fprintf(stdout, "In network %s as %s@%s\n", identity.TeamCode(t.Founder), t.Self.Person, t.Self.Machine)
		return 0
	}
	if pending != "" {
		req, err := identity.DecodeJoinRequest(pending)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "waiting for the founder to admit you; pairing code %s\n", req.PairingCode())
		fmt.Fprintln(stdout, pending)
		return 0
	}

	choice, err := newApprover().Choose(context.Background(), "Set up agentnet",
		"Create a new network, or join one?", []string{"Create a new network", "Join a network"})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var flags []string
	if *person != "" {
		flags = []string{"--person", *person}
	}

	var code int
	switch choice {
	case "Create a new network":
		code = cmdInit(flags)
	case "Join a network":
		code = cmdJoin(flags)
	default:
		fmt.Fprintln(stderr, "not approved")
		return 1
	}
	if code != 0 {
		return code
	}

	fmt.Fprintln(stdout, "Next: run `agentnet setup` to connect Claude Code and Codex.")
	return 0
}

// upStatus reports this machine's current team (nil if none) and any pending join request, without
// creating a database file when neither exists yet.
func upStatus() (t *team.State, pending string, err error) {
	st, err := openStoreIfExists()
	if err != nil || st == nil {
		return nil, "", err
	}
	defer st.Close()
	t, err = team.Load(st)
	if err != nil {
		return nil, "", err
	}
	b, err := st.Get("team", "pending")
	if err != nil {
		return nil, "", err
	}
	return t, string(b), nil
}
