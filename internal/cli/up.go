package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
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

	if teamCode, p, m, ok := currentTeam(); ok {
		fmt.Fprintf(stdout, "In network %s as %s@%s\n", teamCode, p, m)
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

// currentTeam reports this machine's team code, person and machine if it's already in a network.
func currentTeam() (teamCode, person, machine string, ok bool) {
	if _, err := os.Stat(paths.DB()); err != nil {
		return "", "", "", false
	}
	st, err := sqlite.Open(paths.DB())
	if err != nil {
		return "", "", "", false
	}
	defer st.Close()
	t, err := team.Load(st)
	if err != nil || t == nil {
		return "", "", "", false
	}
	return identity.TeamCode(t.Founder), t.Self.Person, t.Self.Machine, true
}
