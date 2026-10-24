# Extending agentnet

agentnet is built around five small interfaces. Each one has exactly one job, and swapping an
implementation never touches the others. Every implementation is wired up in one place:
`internal/cli/daemon.go`.

## `adapter.Adapter` — a coding agent

Defined in `adapter/adapter.go`. One per agent (Claude Code, Codex, a generic pull-only MCP
client).

```go
type Adapter interface {
	Name() string
	Short() string
	EnvSessionID(getenv func(string) string) string
	MetaSessionID(meta map[string]any) string
	Multiplexed() bool
	Channel(s Session, st State) Channel
	Push(ctx context.Context, s Session, text string) error
	HookOutput(ev HookEvent, text string) (out string, ok bool)
	Tier(s Session, st State) Tier
}
```

- `Name`/`Short` identify the adapter (`Short` is used in session names, e.g. `claude`, `codex`).
- `EnvSessionID` and `MetaSessionID` recover a session id from the process environment or from an
  MCP client's reported metadata.
- `Channel` and `Tier` decide, given the daemon's current knowledge of the session (`State`),
  whether and how a message can reach it right now (`ChannelMCP`/`ChannelCommand`/`ChannelHook`/
  `ChannelPull`, ranked `Push` > `Turn` > `Pull`).
- `Push` delivers a message directly (an inbox socket, a CLI command); return `adapter.ErrCannotPush`
  if the adapter has no push mechanism.
- `HookOutput` formats text for a given hook event in the shape that agent's hook contract expects.

Optionally implement `adapter.TurnWatcher` (`TurnRunning`) if the adapter can tell whether a turn
is mid-flight without a Stop hook, and `adapter.ClientNamer` (`ClientNames`) if the agent's MCP
client reports a name other than `Name()`.

**Register it** in `adapters()` in `internal/cli/daemon.go`:

```go
func adapters() *adapter.Registry {
	return adapter.NewRegistry(claude.New(), codex.New(nil), generic.New(), generic.CLI())
}
```

## `store.Store` — persistence

Defined in `store/store.go`. Mailboxes, the outbox, remembered sessions, and small key/value
blobs. The only implementation today is `store/sqlite`.

The interface is large (see `store/store.go` for the full method list: `PutMessage`, `Pending`,
`MarkRead`, `Accept`, `Held`/`ReleaseHeld`, `OutboxAdd`/`OutboxFor`, `UpsertSession`, `Put`/`Get`
for namespaced blobs, and more) because it's the daemon's only persistence boundary — every
durable fact the daemon needs goes through it.

**Test a new Store** with `store/storetest.Run(t, open)`, where `open` returns a fresh `Store` for
each subtest; it exercises the behavior every implementation must have (FIFO mailboxes with
dedup, hold/release, the outbox, session bookkeeping). `store/storetest/sqlite.go` shows the
pattern for a temp-file-backed store.

**Register it** by changing what `sqlite.Open(paths.DB())` resolves to in `newDaemon` in
`internal/cli/daemon.go` — it's passed into `daemon.Options.Store`.

## `transport.Transport` — moving envelopes between machines

Defined in `transport/transport.go`. The only implementation today is `transport/lan` (plain
HTTP). A transport never opens or verifies an envelope; it just moves bytes.

```go
type Transport interface {
	Schemes() []string
	Listen(ctx context.Context, h Handler) (addrs []string, err error)
	Send(ctx context.Context, addr string, e envelope.Envelope) (envelope.Ack, error)
	Close() error
}
```

- `Schemes` names the address schemes it dials (`lan` for `transport/lan`).
- `Listen` starts accepting and calls `h` for each inbound envelope, returning the addresses it's
  now reachable at.
- `Send` delivers one envelope to `addr` and returns the peer's signed `Ack`.
- Return `*transport.RetryAfterError` from `Send` when the peer asked to be retried later.

**Register it** by adding to the `Transports` slice built in `withNetwork` in
`internal/cli/daemon.go`:

```go
o.Transports = []transport.Transport{lan.New("", cfg.Port)}
```

## `discovery.Discoverer` — finding other machines

Defined in `discovery/discovery.go`. Implementations today are `discovery/mdns` (LAN multicast
DNS) and `discovery/static` (a fixed address list, for networks without mDNS — see the README's
"Static peers" section).

```go
type Discoverer interface {
	Announce(ctx context.Context, key []byte, team string, port int) error
	Browse(ctx context.Context, team string, found func(Found)) error
}
```

`Found.Key` may be empty (static peers have no key hint; identity is always proven later by the
envelope's signature, never by what a Discoverer reports).

**Register it** by adding to the `Discoverers` slice in `withNetwork` in
`internal/cli/daemon.go`:

```go
o.Discoverers = []discovery.Discoverer{&mdns.Discoverer{Self: keys.Public()}, static.New(cfg.Peers)}
```

## `seal.Sealer` — encrypting message bodies

Defined in `seal/seal.go`. The only implementation is `seal.HPKE` (DHKEM(X25519) + HKDF-SHA256 +
ChaCha20-Poly1305, RFC 9180, via `crypto/hpke`).

```go
type Sealer interface {
	Seal(recipient []byte, info, plaintext []byte) ([]byte, error)
	Open(priv *ecdh.PrivateKey, info, ciphertext []byte) ([]byte, error)
}
```

**Register it** via `daemon.Options.Sealer` in `newDaemon` in `internal/cli/daemon.go` — leave it
nil to get `seal.HPKE{}` (the daemon's own default).

## `consent.Approver` — asking a human

Defined in `consent/consent.go`. Implementations are `consent.Dialog` (macOS system dialog),
`consent.TTY` (terminal prompt) and `consent.NoHuman` (always refuses — no person to ask).

```go
type Approver interface {
	Confirm(ctx context.Context, title, detail string) (bool, error)
	Choose(ctx context.Context, title, detail string, options []string) (string, error)
}
```

This is the one interface every command that crosses a security boundary (`init`, `join`,
`admit`, `accept`, `revoke`, `setup`, `folder add`) must go through, and the one no MCP tool or
hook may call. See `SECURITY.md` for why this is a speed bump, not a boundary.

**Register it** via the `newApprover` package var in `internal/cli/cli.go`, which calls
`consent.Default(runtime.GOOS, os.Getenv, os.Stdin, os.Stderr)`.
