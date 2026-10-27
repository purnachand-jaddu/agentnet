// agentnet for opencode: written by `agentnet setup`, removed by `agentnet setup --remove`.
// Reports each session's busy/idle state to agentnet, and polls idle sessions to deliver
// teammates' mail into them, on opencode 1.x and 2.x.
import type { Plugin } from "@opencode-ai/plugin"
import { execFile, type ChildProcess } from "node:child_process"

const ENGINE = AGENTNET_ENGINE_PLACEHOLDER

// windowsHide: opencode's background server has no console on Windows, so each hook call (a poll
// every few seconds per idle session) would open a console window of its own that closes at once.
// execFile leaves stdin an open pipe, and agentnet's hook doesn't need it (--session is a flag):
// leaving it open would make the hook wait its whole stdin-read deadline on every call.
const run = (args: string[], cwd?: string) =>
  new Promise<string>((resolve) => {
    const child: ChildProcess = execFile(ENGINE, args, { cwd, timeout: 10_000, windowsHide: true }, (err, out) =>
      resolve(err ? "" : String(out).trim()),
    )
    child.stdin?.end()
  })

// What both plugin formats share; `prompt` puts a teammate's message into a session.
const network = (prompt: (id: string, text: string) => Promise<unknown>) => {
  const idle = new Set<string>()
  const subagents = new Set<string>()
  const delivering = new Set<string>()
  const dirs = new Map<string, string>()
  const dead = new Set<string>() // sessions told to end: no status or delivery may touch them again
  // Mail the daemon already printed to us (and marked read) whose injection failed: retried until
  // the session ends. agentnet's delivery prints then confirms, so a rejected prompt must be ours
  // to recover, not mail silently lost.
  const undelivered = new Map<string, string>()
  // Bumped by every status transition: a delivery observes the stamp when it starts and only the
  // unchanged owner clears the idle flag on completion, or it could erase an idle state that
  // arrived while its prompt was still running.
  const stamps = new Map<string, number>()
  // Status reports are launched in arrival order per session: subprocesses otherwise race, and a
  // late status-busy landing after its idle would leave the daemon showing a busy session.
  const statusChain = new Map<string, Promise<unknown>>()

  const deliver = async (id: string) => {
    if (delivering.has(id) || subagents.has(id) || dead.has(id)) return
    delivering.add(id)
    const stamp = stamps.get(id) ?? 0
    try {
      const polled = undelivered.get(id) ?? (await run(["hook", "opencode", "poll", "--session", id], dirs.get(id)))
      undelivered.delete(id)
      if (!polled) return
      if (!dirs.has(id) || subagents.has(id)) return // the session ended or became a subagent meanwhile
      try {
        await prompt(id, polled)
      } catch {
        undelivered.set(id, polled)
      } finally {
        if ((stamps.get(id) ?? 0) === stamp) {
          idle.delete(id)
        }
      }
    } finally {
      delivering.delete(id)
    }
  }
  const timer = setInterval(() => {
    for (const [id] of [...undelivered]) {
      void deliver(id)
    }
    for (const id of [...idle]) {
      void deliver(id)
    }
  }, 5_000)

  return {
    subagent: (id: string) => subagents.add(id),
    status: async (id: string, state: string, dir: string) => {
      if (subagents.has(id) || dead.has(id)) return
      dirs.set(id, dir)
      stamps.set(id, (stamps.get(id) ?? 0) + 1) // either transition supersedes deliveries in flight
      const next = (statusChain.get(id) ?? Promise.resolve()).catch(() => {}).then(() =>
        run(["hook", "opencode", "status", "--session", id, "--state", state], dir),
      )
      statusChain.set(id, next)
      if (state === "idle") {
        idle.add(id)
        await deliver(id)
      } else idle.delete(id)
    },
    stop: () => clearInterval(timer),
    // Off the network: the session is gone, or opencode unloaded this plugin and nothing delivers
    // to it any more. The session-end is chained behind the statuses queued for that session (they
    // could otherwise run after the end and resurrect it in the daemon), and known sessions end
    // with their folder's cwd so the daemon's folder check passes.
    end: (id?: string) => {
      const finish = (s: string, dir?: string) => {
        const pending = statusChain.get(s)
        statusChain.delete(s)
        void (pending ?? Promise.resolve()).catch(() => {}).then(() =>
          run(["hook", "opencode", "session-end", "--session", s], dir),
        )
      }
      if (id) {
        dead.add(id)
        idle.delete(id)
        undelivered.delete(id)
        stamps.delete(id)
        finish(id, dirs.get(id))
        dirs.delete(id)
        return
      }
      for (const [s, dir] of [...dirs]) {
        dead.add(s)
        idle.delete(s)
        finish(s, dir)
      }
      dirs.clear()
      undelivered.clear()
      stamps.clear()
    },
  }
}

const Agentnet: Plugin = async ({ client, directory }) => {
  // throwOnError: a rejected prompt must throw into our catch (and retry), not resolve as an
  // unnoticed error result — the generated SDK otherwise swallows errors by default.
  const net = network((id, text) =>
    client.session.promptAsync({ path: { id }, body: { parts: [{ type: "text", text }] }, throwOnError: true }),
  )
  return {
    event: async ({ event }) => {
      const e = event as { type?: string; properties?: { info?: { id?: string; parentID?: string }; sessionID?: string; status?: { type?: string } } }
      if (e.type === "session.created" && e.properties?.info?.parentID) net.subagent(e.properties.info.id)
      if (e.type === "session.status") await net.status(e.properties?.sessionID ?? "", e.properties?.status?.type ?? "", directory)
      // 1.x names the deleted session properties.info.id; 2.x-style payloads under 1.x's shape vary.
      if (e.type === "session.deleted") net.end(e.properties?.info?.id ?? e.properties?.sessionID)
    },
    dispose: async () => {
      net.stop()
      net.end()
    },
  }
}

// opencode 2.x has no session.status events; a session's busy and idle come as its execution
// starting and ending. opencode 1.x calls `server`, 2.x calls `setup`.
const STATE: Record<string, string> = {
  "session.execution.started": "busy",
  "session.execution.succeeded": "idle",
  "session.execution.failed": "idle",
  "session.execution.interrupted": "idle",
}

export default {
  id: "agentnet",
  server: Agentnet,
  // The typed context is 2.x's; 1.x calls this too, with a context that has no events, and runs
  // `server` instead.
  setup: (ctx: {
    event?: { subscribe: (opts: { signal: AbortSignal }) => AsyncIterable<{ type?: string; data?: { sessionID?: string; parentID?: string }; location?: { directory?: string } }> }
    location: { directory: string }
    session: { prompt: (args: { sessionID: string; text: string }) => Promise<unknown> }
  }) => {
    // 1.x calls this too, with a context that has no events; it runs `server` instead.
    if (!ctx.event) return
    const abort = new AbortController()
    const net = network((sessionID, text) => ctx.session.prompt({ sessionID, text }))
    void (async () => {
      for await (const e of ctx.event.subscribe({ signal: abort.signal })) {
        const d = e.data ?? {}
        const dir = e.location?.directory ?? ctx.location.directory
        if (e.type === "session.created" && d.parentID) net.subagent(d.sessionID!)
        if (STATE[e.type ?? ""]) void net.status(d.sessionID!, STATE[e.type ?? ""]!, dir)
        if (e.type === "session.deleted") net.end(d.sessionID)
      }
    })().catch(() => {})
    // opencode unloads a folder's plugins when it has been idle a while; its background server
    // stays up, so the daemon can't tell its sessions went away.
    return () => {
      abort.abort()
      net.stop()
      net.end()
    }
  },
}
