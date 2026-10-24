# Architecture

One `agentnet` binary runs two things: a per-machine daemon, and a short-lived CLI/MCP process
that talks to it over a Unix socket. There is no server anywhere else — not a coordination
service, not a relay. Two machines that have joined the same team talk directly over the LAN.

## Processes

- **The daemon** (`agentnet daemon`, usually started by `agentnet setup`'s launchd/systemd
  service) holds everything in memory and in its sqlite store: the session registry, mailboxes,
  the outbox, team state, and the LAN listener. It's the only process that touches the network or
  the store.
- **The MCP server** (`agentnet mcp`) is what Claude Code/Codex/any MCP client runs as a child
  process. It registers the calling session with the daemon and exposes three tools
  (`list_team`, `send_message`, `check_inbox`), dialing the daemon's socket (`ipc`) for each call.
  It never talks to the network or the store directly.
- **Hooks** (`agentnet hook <agent> <event>`) run at an agent's lifecycle points (start, prompt,
  tool use, stop, end) and also talk to the daemon over the socket — this is how a message reaches
  a session that isn't currently connected over MCP.
- **The CLI** (`send`, `who`, `listen`, `folder`, `setup`, `init`, `join`, `admit`, `accept`,
  `revoke`, `doctor`, `up`, `held`, `approve`, `refuse`, `pause`, `resume`) is a person's own
  entry point, also over the same socket.

## Packages, by what they own

| Package | Owns |
|---|---|
| `daemon` | session registry, send/deliver/receive, policy, presence, audit |
| `store` (+ `store/sqlite`) | persistence: mailboxes, outbox, sessions, blobs |
| `adapter` (+ `claude`, `codex`, `generic`) | what's agent-specific: push, hook output, reachability |
| `identity`, `team` | keys, certs, join/admit/accept, revocation |
| `envelope` | signs and HPKE-seals one message or presence update between two machines |
| `seal` | the HPKE primitive envelope uses |
| `transport` (+ `transport/lan`) | moves signed envelopes over the network; never opens them |
| `discovery` (+ `mdns`, `static`) | finds candidate peer addresses; never proves identity |
| `consent` | asks a human; the one boundary no agent can cross |
| `config` | `config.json`: person/machine, folder opt-in, inbound policy, port, static peers |
| `mcpserver`, `hook`, `ipc` | the three ways a session reaches the daemon |
| `internal/cli` | wires all of the above into the `agentnet` binary |

## Joining a team

1. The founder runs `agentnet init`, generating a founder ed25519 keypair (kept at
   `paths.FounderKey()`) and this machine's own member cert.
2. Whoever wants to join runs `agentnet join`, generating a machine keypair and a join request
   containing a pairing code.
3. The joiner sends the join request to the founder out of band; the founder runs
   `agentnet admit <request>`, reads the pairing code back to confirm it's really that person, and
   gets a cert bundle plus the team code to read back.
4. The joiner runs `agentnet accept <bundle>`, checking the team code matches what the founder
   told them, and is now a member: their cert (signed by the founder key) proves it to every other
   member from then on.

Every step but the final signature check is a human confirming a human, over some other channel
(voice, chat) — the pairing code and team code exist so that a correct-looking request/bundle
still gets rejected if nobody actually read the code back.

## Sending a message

1. A session calls `send_message` (MCP) or a hook delivers a reply, or a person runs
   `agentnet send`; all three become a `send` IPC call to the daemon.
2. The daemon resolves the address (`message.Address`: `person/session[@machine]`, or
   `person/*` for a group — only `cli:terminal` may address a group).
3. **Local recipient:** the daemon looks up the live session, picks the best reachable
   `adapter.Channel` (MCP push, a push command, the next hook, or pull), and either pushes it or
   queues it in the mailbox.
4. **Remote recipient:** the daemon builds an `envelope.Envelope` (HPKE-sealed to the recipient's
   machine key, signed with this machine's key), hands it to a `transport.Transport` to send, and
   puts it in the outbox for retry until acknowledged.
5. On receipt, the daemon verifies the signature and cert against team state, checks the clock
   window and replay cache, decrypts, and runs it through the same per-person policy
   (`accept`/`hold`/`refuse`) a local sender would face, before it ever reaches a mailbox.

## Finding peers

`discovery.Discoverer.Browse` reports address hints (mDNS on the LAN, or a fixed list from
`config.Peers` for networks mDNS can't reach, e.g. Tailscale/ZeroTier — see the README). A hint is
never trusted on its own: the envelope's signature against the team's founder key is what proves
who actually sent a message, regardless of where it came from.

## What's not here

No coordination server, no relay, no bring-your-own-URL transport — each is one more
`transport.Transport` and address scheme, deferred by design (see the v1 plan's self-review).
No roles or file permissions beyond founder/member. No cert expiry; revocation is by
`team.Revoke`, which produces an `identity.Block` every member checks on every receive and merge.
