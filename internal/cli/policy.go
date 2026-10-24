package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/daemon"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/message"
)

// cmdHeld runs `agentnet held`: lists every held message.
func cmdHeld(args []string) int {
	fs := flag.NewFlagSet("held", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := autostartDial()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.Close()

	var res daemon.HeldResult
	if err := client.Call(context.Background(), "held", daemon.HeldArgs{}, &res); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, m := range res.Messages {
		fmt.Fprintf(stdout, "%s  %s -> %s  %s\n", m.ID, m.From, m.To, m.First)
	}
	return 0
}

// cmdApprove runs `agentnet approve <id|person>`, gated on consent.
func cmdApprove(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agentnet approve <id|person>")
		return 1
	}
	target := args[0]
	ok, err := newApprover().Confirm(context.Background(), "Approve messages from "+target+"?", target)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	client, err := autostartDial()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.Close()

	a := daemon.ApproveArgs{}
	if message.ValidID(target) {
		a.ID = target
	} else {
		a.Person = target
	}
	var res daemon.ApproveResult
	if err := client.Call(context.Background(), "approve", a, &res); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "released %d\n", res.Released)
	return 0
}

// cmdRefuse runs `agentnet refuse <person>`, gated on consent.
func cmdRefuse(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agentnet refuse <person>")
		return 1
	}
	person := args[0]
	ok, err := newApprover().Confirm(context.Background(), "Refuse messages from "+person+"?", person)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	client, err := autostartDial()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.Close()

	var res daemon.RefusePersonResult
	if err := client.Call(context.Background(), "refuse_person", daemon.RefusePersonArgs{Person: person}, &res); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "dropped %d\n", res.Dropped)
	return 0
}

// cmdPause runs `agentnet pause`: no consent needed, it's a safety brake.
func cmdPause(args []string) int { return setPaused(true, false) }

// cmdResume runs `agentnet resume`, gated on consent.
func cmdResume(args []string) int { return setPaused(false, true) }

// setPaused sets config.Paused and saves it, asking consent first when needConsent.
func setPaused(paused, needConsent bool) int {
	if needConsent {
		ok, err := newApprover().Confirm(context.Background(), "Resume receiving messages?", "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !ok {
			fmt.Fprintln(stderr, "not approved")
			return 1
		}
	}
	if err := paths.Ensure(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfgPath := paths.Config()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg.Paused = paused
	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	verb := "paused"
	if !paused {
		verb = "resumed"
	}
	fmt.Fprintln(stdout, verb)
	return 0
}
