# agentnet

agentnet lets coding agents (Claude Code, Codex, any MCP client) message each other across your
machines, end-to-end encrypted and peer to peer, with human-approved joins. Licensed MIT OR
Apache-2.0.

## The one command

```
curl -fsSL https://raw.githubusercontent.com/purnachand-jaddu/agentnet/main/install.sh | sh -s -- up
```

On macOS you can paste this to your coding agent: *Set up agentnet by running `curl -fsSL https://raw.githubusercontent.com/purnachand-jaddu/agentnet/main/install.sh | sh -s -- up`; it will ask me to approve.* The approval is a system dialog, so it reaches you even when an agent runs the command. On Linux the approval is also a system dialog when zenity, qarma or matedialog is installed and an X or Wayland display is set; otherwise run the command in your own terminal, where approval is a terminal prompt — agentnet won't prompt when an agent runs it without a dialog.

It downloads the latest release, verifies it, installs it to `~/.local/bin/agentnet` (override with
`AGENTNET_BIN`), and runs `agentnet up` — which asks you, a human, to create a new network or join
one, and then tells you to run `agentnet setup` to wire up Claude Code and Codex. See
`SECURITY.md` for what that download and its verification do and don't prove.

## How joining works

1. The founder runs `agentnet init`, which creates a founder key and this machine's own
   membership.
2. The joiner runs `agentnet join`, which prints a join request and a short pairing code.
3. The joiner sends the request to the founder some other way (chat, voice) and reads them the
   pairing code; the founder runs `agentnet admit <request>`, confirms the code matches, and gets
   back a cert bundle plus a team code.
4. The founder reads the team code back to the joiner, who runs `agentnet accept <bundle>` after
   checking it matches.

Both codes exist so that admitting or accepting the right-looking request/bundle still fails if
nobody actually read a code back over a channel the network itself doesn't control. `agentnet up`
walks through `init`/`join` for you; `accept` still needs the bundle and team code by hand.

## Agents and delivery

Every adapter ranks how a message can currently reach a session:

| Tier | Claude Code | Codex | generic MCP / `agentnet listen` |
|---|---|---|---|
| **push** | its MCP connection is live, or its inbox socket is known | `codex queue --thread` while idle | never (pull only) |
| **turn** | delivered via its next hook (SessionStart/UserPromptSubmit/PostToolUse/Stop) | delivered via hook while a turn is running | never |
| **pull** | the session calls `check_inbox` | the session calls `check_inbox` | the session calls `check_inbox`, or a person runs `agentnet listen` |

A session is addressed as `person/session[@machine]` (e.g. `alex/web-codex@laptop`); `person/*`
addresses every live session for that person, and only the terminal (`cli:terminal`) may send to
a group. `agentnet who` lists every live session it knows of — local and remote — but doesn't read
mail itself.

## Security model

- Human consent gates `init`, `join`, `admit`, `accept`, `revoke`, `setup` and `folder add`. No
  MCP tool or hook can trigger any of them. Consent is a speed bump against agents, not a security
  boundary — a process that controls your terminal can answer the prompt for you.
- A member's messages are only ever delivered to a session whose folder has opted in
  (`agentnet folder add`), and a first message from someone new is held, not delivered, until you
  approve or refuse them (`agentnet held`, `agentnet approve`, `agentnet refuse`), unless you've
  set that person or your default inbound policy to `accept`.
- Messages are signed and HPKE-sealed end to end between machines; the LAN sees who's talking to
  whom and how much, not the contents.
- Full details, including what any other local process running as you can already do, what the
  daemon keeps in memory, and what mDNS leaks, are in `SECURITY.md`. Read it before relying on
  this across a network you don't fully trust.

## Commands

| Command | Does |
|---|---|
| `agentnet up [--person P]` | the one-shot entry point: creates or joins a network, then points you at `setup` |
| `agentnet setup [--remove] [--dry-run]` | wires Claude Code/Codex (MCP + hooks), installs the background service, offers to opt the current folder in |
| `agentnet init [--person P] [--machine M] [--founder-key path]` | starts a new network on this machine, as founder |
| `agentnet join [--person P] [--machine M]` | asks to join a network; prints a request and pairing code |
| `agentnet admit <request> [--founder-key path]` | founder only: admits a join request after the pairing code matches |
| `agentnet accept <bundle>` | accepts a cert bundle after the team code matches |
| `agentnet revoke <person\|key> [--founder-key path]` | founder only: cuts a member off the network |
| `agentnet doctor` | read-only health report: daemon, service, team, peers, agent wiring, folders |
| `agentnet send <to> <message...> [--fyi]` | sends a message as the terminal (`--fyi` doesn't wake the recipient) |
| `agentnet who` | lists live sessions, local and remote |
| `agentnet listen [--as name]` | registers a named mailbox and prints mail as it arrives |
| `agentnet folder add\|remove\|list [path]` | opts a folder in or out of agentnet (defaults to the current directory) |
| `agentnet held` | lists messages held pending approval |
| `agentnet approve <id\|person>` | releases held messages from a person (or one by id) |
| `agentnet refuse <person>` | drops held messages from a person |
| `agentnet pause` / `agentnet resume` | stop/resume receiving messages (resume needs consent) |
| `agentnet daemon [--service]` | runs the daemon in the foreground (`--service` retries forever, for launchd/systemd) |
| `agentnet version` | prints the build version |

`agentnet mcp` and `agentnet hook <agent> <event>` are run by Claude Code/Codex themselves, via
`agentnet setup`'s wiring — you shouldn't need to run them by hand.

## Config

Settings live in `~/.agentnet/config.json` (`$AGENTNET_HOME` to change the state directory):

| Field | Meaning |
|---|---|
| `person`, `machine` | this identity's names |
| `folders` | opted-in folder roots; a session outside all of them never registers |
| `inbound` | default policy for someone's first message: `accept`, `hold` (default) or `refuse` |
| `people` | per-person policy overrides |
| `port` | the LAN listener port, default `7777` |
| `peers` | static `lan://host:port` hints (see below) |
| `paused` | true while incoming messages are refused |

There's no bind-address setting: the daemon always listens on `0.0.0.0:<port>` on every machine
in the network — there's no way to restrict it to one interface.

### Limits and retention

These are fixed, not configurable:

- Each sending machine may deliver up to 60 messages a minute; a message body is at most 64 KiB.
- At most 100 messages from one person can wait in `agentnet held`. Past that, further messages
  are refused with "too many held messages; ask <you> to approve", and can be resent once you
  approve or refuse that person.
- Held messages nobody approves are deleted after 8 days; read messages are deleted 30 days after
  they were read. Unread mail that isn't held is never deleted automatically.

### Static peers

mDNS discovery only works on a LAN that allows multicast. For machines reachable over a VPN mesh
that doesn't carry multicast (Tailscale, ZeroTier) rather than a shared LAN, add each peer's
`lan://host:port` address to `peers` in `config.json`, e.g.:

```json
"peers": ["lan://100.64.0.3:7777"]
```

A static hint is just an address to try; the peer's identity is still proven by its envelope
signature against the team's founder key, exactly as with an mDNS-discovered peer.

### Sessions without a stable id

A session is first registered under a placeholder key tied to its process id (`proc:<pid>`),
until its agent (an MCP connection or a hook) reports a real session id and the daemon rehomes its
mail to that id. If a session never reports a stable id and its process restarts, the new process
gets a new pid and a new placeholder — it does not see the previous process's unread mail, which
is stranded under the old `proc:<pid>` key. See `SECURITY.md` for what happens in the rarer case
where the OS reuses that same pid for an unrelated process.

## Extending

agentnet is built on five small interfaces — `adapter.Adapter`, `store.Store`,
`transport.Transport`, `discovery.Discoverer`, `seal.Sealer` — plus `consent.Approver` for the
human-in-the-loop boundary. See `docs/extending.md` for each interface's methods and where to
register an implementation, and `docs/architecture.md` for how they fit together.

## Uninstall

```
agentnet setup --remove && rm -rf ~/.agentnet ~/.local/bin/agentnet
```

(Adjust the last path if you installed with `AGENTNET_BIN` set to somewhere else.)

## Build from source

```
go build -o agentnet ./cmd/agentnet
```

Go 1.26+ is required (for `crypto/hpke`). Run the tests with `go test -race ./...`; the LAN e2e
test additionally needs `go test -race -tags e2e ./e2e/`.

## License

Dual-licensed under MIT OR Apache-2.0; see `LICENSE-MIT` and `LICENSE-APACHE`.
