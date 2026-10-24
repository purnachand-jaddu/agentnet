# Security

agentnet lets coding agents on machines you control message each other. This is its threat model
and its limits — read it before you trust it with anything.

## Consent is a speed bump, not a boundary

`init`, `join`, `admit`, `accept`, `revoke`, `setup` and `folder add` all go through
`consent.Approver` (a system dialog on macOS, a TTY prompt elsewhere). No MCP tool or hook can
trigger them. But the approver only proves a human is present at the machine right now — it does
not prove the process asking is trustworthy. Any process that can control your terminal or your
screen (including a misbehaving or prompt-injected agent with shell access) can answer the prompt
for you. Don't treat an approval as proof of informed consent; treat it as a reminder to look.

## What any local, same-user process can do

The daemon's Unix socket lives under `paths.Home()` (`~/.agentnet` by default, mode 0700), so a
different user on the same machine can't reach it. Any process running *as you*, agent or not,
can:

- Dial the socket and call `send` with `From: "cli:terminal"`. That string is the daemon's own
  marker for "a human typed this at a prompt" — it bypasses the loop guard and the "only the
  terminal can message a group" restriction by design, because the daemon has no way to tell a
  real terminal from a script that spoofs it. Only agentnet's own `agentnet send` CLI command
  used to set this value; anything else on your machine can set it too.
- Call the `approve` and `refuse_person` IPC operations directly, with no consent check — the CLI
  (`agentnet approve`/`agentnet refuse`) asks for consent before calling them, but the daemon
  itself does not gate the operations. Consent for holds is enforced CLI-side only.

Both are consequences of same-machine, same-user trust: if a process can run as you, it can drive
agentnet as you.

## What the daemon keeps, and for how long

Claude Code session messaging tokens (used to push a message into a live Claude session) are held
in memory only, for as long as the daemon runs, and are never written to disk.

A session identified only by its process id, with no agent-reported stable session id (the
placeholder keys the daemon uses before an MCP or hook registration supplies a real one), does not
carry mail forward to whatever process reuses that pid next. If that process exits and a new,
unrelated one starts later, any mail addressed to the old placeholder is not handed to it — there
is no linkage beyond the pid matching while the original process is still the one running.

## What's visible on the LAN

Envelopes travel over plain HTTP (no TLS) between machines on the same network. For every message
and presence update, an observer on the LAN can see:

- the sender's cert: person name, machine name, team id;
- the recipient's machine key;
- the message kind (`msg` or `presence`) and its id and timestamp;
- the ciphertext size.

They cannot see the message body or any session content — bodies are HPKE-sealed
(X25519 + HKDF-SHA256 + ChaCha20-Poly1305, RFC 9180) to the recipient's encryption key, and the
seal's AAD binds it to both parties' signing keys so it can't be stripped and re-sent as someone
else's message. What leaks is metadata: who is talking to whom, how often, and how much.

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

`install.sh` is fetched from `main` over TLS (`curl -fsSL --proto '=https' ... | sh`) — that step
trusts GitHub and your network path to it, same as any curl-pipe-to-shell installer. From there:

- the release archive's sha256 checksum is verified against `checksums.txt` from the same GitHub
  release — this proves the download wasn't corrupted or substituted *in transit*, but both files
  come from the same place, so it's integrity, not provenance;
- if `gh` is installed, `install.sh` additionally runs `gh attestation verify` against the
  archive, which checks GitHub's Sigstore-backed proof that the binary was built by this repo's
  own release workflow — that's provenance. Without `gh`, you get integrity only.

If you don't trust the network path or GitHub itself, don't pipe this to `sh`: download the
release, verify it the same way by hand, or build from source.

## Reporting a vulnerability

Please report security issues via [GitHub private security advisories](https://github.com/purnachand-jaddu/agentnet/security/advisories/new)
rather than a public issue.
