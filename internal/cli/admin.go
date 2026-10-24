package cli

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/internal/paths"
	"github.com/purnachand-jaddu/agentnet/ipc"
	"github.com/purnachand-jaddu/agentnet/store/sqlite"
	"github.com/purnachand-jaddu/agentnet/team"
)

// resolveRevokeTarget finds every member cert target names: every cert of that person, or, failing
// that, the one member whose key's base64url form target exactly matches or uniquely prefixes.
func resolveRevokeTarget(tm *team.State, target string) ([]identity.Cert, error) {
	if certs := tm.ByPerson(target); len(certs) > 0 {
		return certs, nil
	}
	var matches []identity.Cert
	for _, c := range tm.MembersSnapshot() {
		if target != "" && strings.HasPrefix(base64.RawURLEncoding.EncodeToString(c.Key), target) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no member matches %q", target)
	case 1:
		return matches, nil
	default:
		return nil, fmt.Errorf("key prefix %q matches more than one member", target)
	}
}

// revokeConsentText lists every cert about to be revoked, per the decision's exact phrasing.
func revokeConsentText(certs []identity.Cert) string {
	parts := make([]string, len(certs))
	for i, c := range certs {
		parts[i] = fmt.Sprintf("%s@%s (key %s)", c.Person, c.Machine, base64.RawURLEncoding.EncodeToString(c.Key)[:12])
	}
	return "Revoke " + strings.Join(parts, ", ") + "? They will be cut off from the network."
}

// notifyDaemonTeamChanged tells a running daemon to reload team state and gossip immediately; a daemon
// that isn't running is not an error (it will pick up the change from disk next time it starts).
func notifyDaemonTeamChanged() {
	c, err := ipc.Dial(paths.Socket())
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.Call(context.Background(), "team_changed", nil, nil)
}

// cmdRevoke runs `agentnet revoke <person|key> [--founder-key path]`, founder only, with consent.
func cmdRevoke(args []string) int {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	founderKeyFlag := fs.String("founder-key", "", "where the founder key lives")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agentnet revoke <person|key> [--founder-key path]")
		return 2
	}
	target := fs.Arg(0)

	founderKeyPath := paths.FounderKey()
	if *founderKeyFlag != "" {
		founderKeyPath = *founderKeyFlag
	}
	founderPriv, err := loadFounderKey(founderKeyPath)
	if err != nil {
		fmt.Fprintf(stderr, "only the founder can revoke people: %v\n", err)
		return 1
	}

	st, err := sqlite.Open(paths.DB())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()

	tm, err := team.Load(st)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if tm == nil {
		fmt.Fprintln(stderr, "team: no network on this machine")
		return 1
	}

	certs, err := resolveRevokeTarget(tm, target)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, c := range certs {
		if c.Admin {
			fmt.Fprintln(stderr, "you can't revoke the founder's own machine")
			return 1
		}
	}

	ok, err := newApprover().Confirm(context.Background(), revokeConsentText(certs), "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "not approved")
		return 1
	}

	now := time.Now()
	for _, c := range certs {
		if _, err := team.Revoke(st, founderPriv, tm, c.Key, "revoked by founder", now); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	notifyDaemonTeamChanged()

	for _, c := range certs {
		fmt.Fprintf(stdout, "revoked %s@%s\n", c.Person, c.Machine)
	}
	return 0
}
