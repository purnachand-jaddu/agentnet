# Security

agentnet lets coding agents on machines you control message each other. This is its threat model
and its limits — read it before you trust it with anything.

## Consent is a speed bump, not a boundary

`init`, `join`, `admit`, `accept`, `revoke`, `setup` and `folder add` all go through
`consent.Approver` (a system dialog on macOS; elsewhere a TTY prompt in your own terminal, else on
Linux a zenity, qarma or matedialog dialog when a display is set). No MCP tool or hook can
trigger them. But the approver only proves a human is present at the machine right now — it does
not prove the process asking is trustworthy. Any process that can control your terminal or your
screen (including a misbehaving or prompt-injected agent with shell access) can answer the prompt
for you. Don't treat an approval as proof of informed consent; treat it as a reminder to look.

On Linux the dialog also appears when an agent runs the command. Under X11 any local process,
including the agent (with `xdotool`, say), can answer it. Wayland doesn't stop this either: the
agent controls the dialog's environment, so it can point it at XWayland or its own Xvfb. On a
machine with a display but nobody at it, the command waits up to five minutes, then denies. An
agent can also put a fake dialog program (`zenity`, or `osascript` on macOS) first on `PATH` to
answer for you; that is a known limit, consistent with the same-user trust below.

## What any local, same-user process can do

The daemon's Unix socket lives under `paths.Home()` (`~/.agentnet` by default, mode 0700) — or,
when that path is too long for a Unix socket, under a `$TMPDIR/agentnet-<uid>` fallback directory
that's created 0700 and checked to still be owned by you before use. Either way, a different user
on the same machine can't reach it. Any process running *as you*, agent or not, can:

- Dial the socket and call `send` with `From: "cli:terminal"`. That string is the daemon's own
  marker for "a human typed this at a prompt" — it bypasses the loop guard and the "only the
  terminal can message a group" restriction by design, because the daemon has no way to tell a
  real terminal from a script that spoofs it. Only agentnet's own `agentnet send` CLI command is
  meant to set this value; anything else on your machine can set it too.
- Call the `approve` and `refuse_person` IPC operations directly, with no consent check — the CLI
  (`agentnet approve`/`agentnet refuse`) asks for consent before calling them, but the daemon
  itself does not gate the operations. Consent for holds is enforced CLI-side only.
- Edit `~/.agentnet/config.json` directly (`folders`, `inbound`, `people`, `paused`), which
  bypasses every CLI consent prompt, including `folder add` and `resume`.

The network side of the first point: a receiving machine trusts the sending machine's claim that
a message came from `person/terminal`. So a terminal send, including an `agentnet send` that a
prompt-injected agent runs, skips the receiver's loop guard and may address a group there too.
Only the receiver's limit of 60 messages a minute per sending machine still applies.

All of these are consequences of same-machine, same-user trust: if a process can run as you, it
can drive agentnet as you.

## The opencode plugin

`agentnet setup` writes `~/.config/opencode/plugins/agentnet.ts`. OpenCode autoloads every `.ts`
in that directory and runs it with your user's privileges inside its own process. The plugin does
only three things: it reports each session's busy/idle state and lifecycle to the daemon (running
`agentnet hook opencode …`, which dial's the daemon's socket like any other hook), asks the daemon
for pending mail while a session is idle, and injects whatever mail comes back into that session
via OpenCode's prompt API — where it arrives framed as peer input (`<agentnet-message>`), the same
as every other agent's delivery. The plugin decides nothing about who may send: folder opt-in,
hold-for-approval and the loop guard all stay in the daemon.

Two corollaries of the above, both standard same-user trust:

- Any process running as you can drop its own `.ts` into that plugins directory, and OpenCode will
  run it the same way it runs ours. Setup refuses to overwrite a `plugins/agentnet.ts` that doesn't
  carry its marker (save it or move it first), and `setup --remove` deletes only the file carrying
  its marker — strangers' files stay.
- The plugin runs the installed `agentnet` binary by path. Replacing that binary replaces what the
  plugin executes — same as replacing any binary your hooks run.

## What the daemon keeps, and for how long

Messages are stored in `~/.agentnet/agentnet.db` (mode 0600), unencrypted at rest. Held messages
nobody approves are deleted after 8 days, read messages 30 days after they were read, and at most
100 messages per sending person can be held at once (further ones are refused, not stored). Unread
mail that isn't held is never deleted automatically, so a member you accept messages from can
still fill your disk at up to 60 messages (64 KiB each) a minute per machine until you
`agentnet refuse` them or `agentnet pause`.

Claude Code session messaging tokens (used to push a message into a live Claude session) are held
in memory only, for as long as the daemon runs, and are never written to disk.

A session that never reports a stable id is tracked under a placeholder key tied to its process
id (`proc:<pid>`), until an MCP or hook registration supplies a real one. The real limitation is
the common case: if that process restarts, the new process gets a new pid and a new placeholder,
and never sees the old one's unread mail — it's stranded under the old key.

The rarer case cuts the other way, and is not currently guarded against: nothing clears a
placeholder's mailbox when its session unregisters (`daemon/sessions.go`'s `unregister` only
drops the in-memory `live` entry, not the stored mail). If the OS reuses that exact pid for an
unrelated process before the old mail is read, and that process also registers under the same
agent and pid, it can see the previous process's leftover unread mail. This is a real, not
theoretical, leak path; it just requires the specific pid to be reused for the same agent name
before anything else claims it.

## What's visible on the LAN

Envelopes travel over plain HTTP (no TLS) between machines on the same network. For every message
and presence update, an observer on the LAN can see:

- the sender's cert: person name, machine name, team id;
- the recipient's machine key;
- the message kind (`msg` or `presence`) and its id and timestamp;
- the ciphertext size.

They cannot see the message body or any session content — bodies are HPKE-sealed
(X25519 + HKDF-SHA256 + ChaCha20-Poly1305, RFC 9180) to the recipient's encryption key, and the
HPKE info binds the seal to both parties' signing keys so it can't be stripped and re-sent as
someone else's message. What leaks is metadata: who is talking to whom, how often, and how much.

mDNS discovery broadcasts, in clear, on the local network: the machine's public signing key, an
HMAC-derived tag of the team id (not the team id itself, but a fixed value per team so machines on
the same team are linkable to each other), and — because the underlying mDNS library announces a
hostname — the machine's hostname. A passive observer on the LAN can tell that a given machine
runs agentnet, which team it's on, and link its presence over time, without decrypting anything.

## The founder key

The founder key (at `paths.FounderKey()`, `~/.agentnet/founder.key` unless `init --founder-key`
points elsewhere) signs every member certificate and every revocation. It is **online by default**
— readable by the founder's own user account on the founder's machine, no extra protection. Moving
it offline (a USB key, a different machine, `--founder-key` pointing elsewhere) is recommended but
not automatic. Anyone who reads that file can admit or revoke members and mint certificates for
your team.

## Peer input is never trusted as the user's

Every message from a peer agent is delivered to the receiving session framed as peer input — an
`<agentnet-message>`, explicitly not the session's own user. A compromised or prompt-injected
member can still send text, but it arrives labeled as untrusted, and can only act within whatever
limits the *receiving* agent already has (folder opt-in, its own tool permissions, its own
judgment). agentnet doesn't grant a sender any new capability on the receiver's machine.

## `install.sh`'s own trust chain

`install.sh` is fetched from `main` over TLS
(`curl -fsSL https://raw.githubusercontent.com/purnachand-jaddu/agentnet/main/install.sh | sh -s -- up`)
— that step trusts GitHub and your network path to it, same as any curl-pipe-to-shell installer.
From there:

- the release archive's sha256 checksum is verified against `checksums.txt` from the same GitHub
  release — this proves the download wasn't corrupted or substituted *in transit*, but both files
  come from the same place, so it's integrity, not provenance;
- if `gh` is installed and logged in, `install.sh` additionally runs `gh attestation verify`
  against the archive, which checks GitHub's Sigstore-backed proof that the binary was built by
  this repo's own release workflow — that's provenance. Without `gh` installed and logged in, you
  get integrity only (and `install.sh` says so on stderr rather than failing).

If you don't trust the network path or GitHub itself, don't pipe this to `sh`: download the
release, verify it the same way by hand, or build from source.

The release workflow publishes the archives and `checksums.txt` first, then runs the attestation
step as a separate, later step. If attestation fails or the workflow is interrupted between the
two, the release stays live with checksums but no attestation for a window of time — every
`gh`-authenticated install during that window fails closed (checksum passes, attestation doesn't
exist yet, so verification fails) rather than silently skipping the check.

## Reporting a vulnerability

Please report security issues via [GitHub private security advisories](https://github.com/purnachand-jaddu/agentnet/security/advisories/new)
rather than a public issue.
