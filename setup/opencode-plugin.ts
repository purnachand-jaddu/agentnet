// agentnet for opencode: written by `agentnet setup`, removed by `agentnet setup --remove`.
// Reports each session's busy/idle state to agentnet, and polls idle sessions to deliver
// teammates' mail into them, on opencode 1.x and 2.x.
import type { Plugin } from "@opencode-ai/plugin"
import { execFile, type ChildProcess } from "node:child_process"

const ENGINE = AGENTNET_ENGINE_PLACEHOLDER

// windowsHide: opencode's background server has no console on Windows, so each hook call (a poll
// every few seconds per idle session) would open a console window of its own that closes at once.
const run = (args: string[], cwd: string) =>
  new Promise<string>((resolve) => {
    const child: ChildProcess = execFile(ENGINE, args, { cwd, timeout: 10_000, windowsHide: true }, (err, out) =>
      resolve(err ? "" : String(out).trim()),
    )
    // execFile leaves stdin as an open pipe; agentnet's hook doesn't need it (--session is a flag),
    // and an open pipe would make the hook wait its full stdin-read deadline on every call.
    child.stdin?.end()
  })

// What both plugin formats share; `prompt` puts a teammate's message into a session.
const network = (prompt: (id: string, text: string) => Promise<unknown>) => {
  const idle = new Set<string>()
  const subagents = new Set<string>()
  const delivering = new Set<string>()
  const dirs = new Map<string, string>()
  // Mail the daemon already printed to us (and marked read) whose injection failed: retried until
  // the session ends. agentnet's delivery prints then confirms, so a rejected prompt must be ours
  // to recover, not mail silently lost.
  const undelivered = new Map<string, string>()

  const deliver = async (id: string) => {
    if (delivering.has(id) || subagents.has(id)) return
    delivering.add(id)
    try {
      const polled = undelivered.get(id) ?? (await run(["hook", "opencode", "poll", "--session", id], dirs.get(id)!))
      undelivered.delete(id)
      if (!polled) return
      // The session may have ended while the poll ran; mail for a session opencode forgot would
      // only fail again.
      if (!dirs.has(id)) return
      try {
        await prompt(id, polled)
      } catch {
        undelivered.set(id, polled)
      } finally {
        idle.delete(id)
      }
    } finally {
      delivering.delete(id)
    }
  }
  const timer = setInterval(() => {
    for (const [id, text] of [...undelivered]) {
      void deliver(id)
    }
    for (const id of [...idle]) {
      void deliver(id)
    }
  }, 5_000)

  return {
    subagent: (id: string) => subagents.add(id),
    status: async (id: string, state: string, dir: string) => {
      if (subagents.has(id)) return
      dirs.set(id, dir)
      void run(["hook", "opencode", "status", "--session", id, "--state", state], dir)
      if (state === "idle") {
        idle.add(id)
        await deliver(id)
      } else idle.delete(id)
    },
    stop: () => clearInterval(timer),
    // Off the network: the session is gone, or opencode unloaded this plugin and nothing delivers to it any more.
    end: (id?: string) => {
      for (const [s, dir] of [...dirs]) {
        if (id && s !== id) continue
        idle.delete(s)
        dirs.delete(s)
        void run(["hook", "opencode", "session-end", "--session", s], dir)
      }
      if (id) undelivered.delete(id)
      else undelivered.clear()
    },
  }
}

const Agentnet: Plugin = async ({ client, directory }) => {
  const net = network((id, text) =>
    client.session.promptAsync({ path: { id }, body: { parts: [{ type: "text", text }] } }),
  )
  return {
    event: async ({ event }) => {
      const e = event as { type?: string; properties?: { info?: { id?: string; parentID?: string }; sessionID?: string; status?: { type?: string } } }
      if (e.type === "session.created" && e.properties?.info?.parentID) net.subagent(e.properties.info.id)
      if (e.type === "session.status") await net.status(e.properties?.sessionID ?? "", e.properties?.status?.type ?? "", directory)
      if (e.type === "session.deleted") net.end(e.properties?.sessionID)
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
