// Package cli is agentnet's command-line entry: daemon, mcp, hook, send, who, listen, folder, setup,
// init, join, admit, accept, up, held, approve, refuse, pause, resume, version.
package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/purnachand-jaddu/agentnet/consent"
)

// stdout, stderr and stdin are package vars so tests can redirect them.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	stdin  io.Reader = os.Stdin
)

// newApprover builds the consent.Approver for folder add/remove; tests override it.
var newApprover = func() consent.Approver {
	return consent.Default(runtime.GOOS, os.Getenv, os.Stdin, os.Stderr)
}

// Main runs the agentnet CLI for args (os.Args[1:]) and returns the process exit code.
func Main(args []string, version string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "daemon":
		return cmdDaemon(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "hook":
		return cmdHook(args[1:])
	case "send":
		return cmdSend(args[1:])
	case "who":
		return cmdWho(args[1:])
	case "listen":
		return cmdListen(args[1:])
	case "folder":
		return cmdFolder(args[1:])
	case "setup":
		return cmdSetup(args[1:])
	case "init":
		return cmdInit(args[1:])
	case "join":
		return cmdJoin(args[1:])
	case "admit":
		return cmdAdmit(args[1:])
	case "accept":
		return cmdAccept(args[1:])
	case "up":
		return cmdUp(args[1:])
	case "held":
		return cmdHeld(args[1:])
	case "approve":
		return cmdApprove(args[1:])
	case "refuse":
		return cmdRefuse(args[1:])
	case "pause":
		return cmdPause(args[1:])
	case "resume":
		return cmdResume(args[1:])
	default:
		usage()
		return 2
	}
}

// usage prints the command summary to stderr.
func usage() {
	fmt.Fprintln(stderr, "usage: agentnet daemon|mcp|hook|send|who|listen|folder|setup|init|join|admit|accept|up|held|approve|refuse|pause|resume|version")
}
