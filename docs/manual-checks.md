# Manual checks: Phase 1 on a real Mac

The e2e test (`e2e/local_test.go`) proves the daemon, hooks and `send`/`who` work with fake
agents. These checks need a real Claude Code and a real Codex, so they're manual. Run each
after `agentnet setup` on a real machine, and fill in the "Result:" line.

## 1. Claude Code: one session, hook+MCP joined by PID

Open one Claude Code conversation in a folder agentnet is on for. Run `agentnet who` while it's
active.

Expect: exactly ONE row for that conversation (not two), with a push tier. This checks that
`CLAUDE_CODE_SESSION_ID` and `CLAUDE_CODE_MESSAGING_SOCKET` reach the MCP child process, and that
the hook's PID equals the MCP's ParentPID once `proc.AgentPID` walks past any shell wrapper.

Result:

## 2. Codex: hook session_id works as `--thread`

Open a Codex session in a folder agentnet is on for. Note the session id agentnet assigns it
(`agentnet who`). Run `codex queue --thread <that id> --message=test` by hand from a terminal.

Expect: the message is queued without error and shows up in that Codex session.

Result:

## 3. Claude to Codex, idle and mid-turn

With a Codex session idle, `agentnet send` it a message from the Claude session (or
`agentnet send <codex-address> hello` from a terminal). Expect it to appear right away.

Then start a long Codex turn and send again mid-turn.

Expect: the message arrives at the next tool call or at the Stop hook, not immediately.

Result:

## 4. Codex to Claude, Claude idle

With the Claude Code session idle, send it a message from Codex (or `agentnet send
<claude-address> hello` from a terminal).

Expect: Claude wakes and starts a turn on its own.

Result:

## 5. Daemon-side direct post to a bypass-permissions Claude session

Temporarily remove the MCP server from Claude Code's config so it has no `check_inbox`/push
path, restart the session in `--dangerously-skip-permissions` (bypass) mode, then have the
daemon post directly to its inbox socket (e.g. `agentnet send <its address> hello`).

Expect: record whether the message is held for approval or delivered straight through, and
which `crossSessionInbound` config setting (if any) changes that behavior.

Result:

## 6. `/clear` doesn't leak mail across conversations

Send a message to a Claude Code session, let it arrive, then run `/clear`. Send another message
to the same address afterward.

Expect: the pre-`/clear` conversation's mail does not appear in the new one.

Result:

## 7. macOS consent dialog: approve, deny, give up

Run `agentnet folder add` on a new folder three times: once approving the dialog, once denying
it, and once waiting 5 minutes without answering.

Expect: approve adds the folder; deny exits with "not approved"; waiting 5 minutes times out
without adding the folder.

Then run `agentnet folder add` from inside a Claude Code Bash tool call.

Expect: the dialog appears on screen, but the agent itself cannot see or answer it (it's a
human-only speed bump, not something the agent can click through).

Result:

## 8. `setup` and `setup --remove` are idempotent

Run `agentnet setup` twice in a row, then `agentnet setup --remove` twice in a row.

Expect: both repeats are no-ops (no duplicate entries, no errors), and after the removes,
Claude/Codex configs and the installed service are all gone. After `setup`, `launchctl print
gui/$(id -u)/dev.agentnet.daemon` shows the service running.

Result:

## 9. Daemon restart, MCP redials

With a Claude Code session open and connected, run `launchctl kickstart -k
gui/$(id -u)/dev.agentnet.daemon` to restart the daemon.

Expect: the open Claude session still receives pushes within a few seconds, i.e. its MCP
process notices the dead connection and redials on its own.

Result:

## 10. Linux systemd (if available)

On a Linux box, run `agentnet setup` with a systemd user service available, and again with no
user bus (e.g. over SSH with no login session).

Expect: the service installs and runs when a user bus is present; note what happens (fallback,
clear error, or silent no-op) when it isn't.

Result:
