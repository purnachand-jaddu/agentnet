package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/daemon"
)

// cmdSend runs `agentnet send <to> <message...> [--fyi]`.
func cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fyi := fs.Bool("fyi", false, "don't wake the recipient")
	fs.SetOutput(stderr)

	// --fyi is documented as trailing, but flag.Parse stops at the first non-flag arg; strip it
	// out wherever it appears so both `send --fyi to msg` and `send to msg --fyi` work.
	var rest []string
	for _, a := range args {
		if a == "--fyi" {
			continue
		}
		rest = append(rest, a)
	}
	hadFyi := len(rest) != len(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if hadFyi {
		*fyi = true
	}

	pos := fs.Args()
	if len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: agentnet send <to> <message...> [--fyi]")
		return 1
	}
	to, body := pos[0], strings.Join(pos[1:], " ")

	client, err := autostartDial()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.Close()

	var res daemon.SendResult
	a := daemon.SendArgs{From: "cli:terminal", To: to, Body: body, Wake: !*fyi}
	if err := client.Call(context.Background(), "send", a, &res); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, r := range res.Receipts {
		fmt.Fprintf(stdout, "%s: %s — %s\n", r.To, r.State, r.Note)
	}
	return 0
}

// cmdWho runs `agentnet who`.
func cmdWho(args []string) int {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
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

	var res daemon.WhoResult
	if err := client.Call(context.Background(), "who", daemon.WhoArgs{}, &res); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, e := range res.Sessions {
		line := fmt.Sprintf("%s  %s  %s", e.Address, e.Agent, e.Status)
		if e.You {
			line += "  ← you"
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// listenWaitSecs bounds each long-poll in cmdListen's loop.
const listenWaitSecs = 30

// cmdListen runs `agentnet listen [--as name]`: registers a cli session, then prints mail as it arrives.
func cmdListen(args []string) int {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	as := fs.String("as", "terminal", "the name to register as")
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

	cwd, _ := os.Getwd()
	key := "cli:" + *as
	reg := daemon.RegisterArgs{Session: adapter.Session{Key: key, Agent: "cli", ID: *as, Cwd: cwd}, Name: *as}
	var out daemon.Registered
	if err := client.Call(context.Background(), "register", reg, &out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "listening as %s\n", out.Address)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for ctx.Err() == nil {
		var res daemon.InboxResult
		if err := client.Call(ctx, "inbox", daemon.InboxArgs{Key: key, WaitSecs: listenWaitSecs, Peek: true}, &res); err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		if res.Text == "" {
			continue
		}
		// Only confirm delivery once the mail was actually shown; a print failure must leave it unread.
		if _, err := fmt.Fprintln(stdout, res.Text); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if len(res.IDs) > 0 {
			client.Call(context.Background(), "delivered", daemon.DeliveredArgs{Key: key, IDs: res.IDs}, nil)
		}
	}
	return 0
}
