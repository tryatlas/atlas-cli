// Atlas plugin for OpenCode — written by atlas agents add; do not edit (rewritten on the next add).
//
// Forwards OpenCode's session lifecycle to `atlas hooks opencode <verb>` with
// one JSON payload on stdin. node:child_process runs under Bun (the CLI and
// TUI) and Node (the desktop sidecar). A machine without atlas on PATH runs a
// silent no-op, and nothing here throws: a failing hook never fails OpenCode.
import { spawn, spawnSync } from "node:child_process"
import type { Plugin } from "@opencode-ai/plugin"

export const AtlasPlugin: Plugin = async ({ directory }) => {
  let session: string | null = null // the session these events belong to
  let model = "" // modelID of the latest assistant message
  let turnOpen = false // a prompt went out and no idle has followed it
  let pending: { messageID: string; sessionID: string } | null = null // a user message awaiting its text part
  const children = new Set<string>() // sub-agent sessions, which are not turns of ours
  const roles = new Map<string, string>() // message id → role
  const early = new Map<string, string>() // user text seen before its message.updated
  const started = new Set<string>() // user message ids whose turn-start fired

  function argv(verb: string): string[] {
    return ["sh", "-c", `if ! command -v atlas >/dev/null 2>&1; then exit 0; fi; exec atlas hooks opencode ${verb}`]
  }

  // send runs a hook in the background; its exit is never waited for.
  function send(verb: string, payload: Record<string, unknown>) {
    try {
      const [cmd, ...args] = argv(verb)
      const child = spawn(cmd, args, { cwd: directory, stdio: ["pipe", "ignore", "ignore"] })
      child.on("error", () => {})
      child.stdin?.on("error", () => {})
      child.stdin?.end(JSON.stringify(payload) + "\n")
    } catch {
      // never fail OpenCode
    }
  }

  // sendSync waits for the hook: turn-start must record its baseline before
  // the agent edits files, and turn-end and session-end must finish before
  // `opencode run` tears the process down on the same idle event.
  function sendSync(verb: string, payload: Record<string, unknown>) {
    try {
      const [cmd, ...args] = argv(verb)
      spawnSync(cmd, args, { cwd: directory, input: JSON.stringify(payload) + "\n", stdio: ["pipe", "ignore", "ignore"], timeout: 60_000 })
    } catch {
      // never fail OpenCode
    }
  }

  function reset() {
    session = null
    model = ""
    turnOpen = false
    pending = null
    roles.clear()
    early.clear()
    started.clear()
  }

  // follow makes id the tracked session; it reports whether that is new.
  function follow(id: string): boolean {
    if (session === id) return false
    reset()
    session = id
    return true
  }

  function startTurn(messageID: string, sessionID: string, prompt: string) {
    if (started.has(messageID)) return
    started.add(messageID)
    if (pending?.messageID === messageID) pending = null
    turnOpen = true
    sendSync("turn-start", { session_id: sessionID, prompt, model })
  }

  return {
    event: async ({ event }) => {
      try {
        const props = (event as any).properties
        switch (event.type) {
          case "session.created": {
            const info = props?.info
            if (!info?.id) break
            if (info.parentID) {
              children.add(info.id)
              break
            }
            if (follow(info.id)) send("session-start", { session_id: info.id })
            break
          }

          case "message.updated": {
            const info = props?.info
            if (!info?.id || children.has(info.sessionID)) break
            // A resumed session fires no session.created; its first message
            // is where it begins for us.
            if (info.sessionID && follow(info.sessionID)) sendSync("session-start", { session_id: info.sessionID })
            roles.set(info.id, info.role)
            if (info.role === "assistant" && info.modelID) model = info.modelID
            if (info.role === "user") {
              // The user message arrives before its text part; the turn
              // starts on the part (with the prompt), or on the session
              // turning busy when no text part follows.
              const sessionID = info.sessionID ?? session
              if (!sessionID) break
              const prompt = early.get(info.id)
              early.delete(info.id)
              if (prompt !== undefined) startTurn(info.id, sessionID, prompt)
              else pending = { messageID: info.id, sessionID }
            }
            break
          }

          case "message.part.updated": {
            const part = props?.part
            if (!part?.messageID || part.type !== "text" || children.has(part.sessionID)) break
            const role = roles.get(part.messageID)
            if (role === undefined) {
              if (early.size > 64) early.clear()
              early.set(part.messageID, part.text ?? "")
              break
            }
            if (role !== "user") break
            const sessionID = part.sessionID ?? session
            if (sessionID) startTurn(part.messageID, sessionID, part.text ?? "")
            break
          }

          case "session.status": {
            const status = props?.status?.type
            const sessionID = props?.sessionID ?? session
            if (!sessionID || children.has(sessionID)) break
            if (status === "busy") {
              if (pending && pending.sessionID === sessionID) startTurn(pending.messageID, sessionID, "")
              break
            }
            if (status !== "idle" || !turnOpen) break
            turnOpen = false
            sendSync("turn-end", { session_id: sessionID, model })
            break
          }

          case "session.compacted": {
            const sessionID = props?.sessionID
            if (!sessionID || children.has(sessionID)) break
            send("compaction", { session_id: sessionID })
            break
          }

          case "session.deleted": {
            const info = props?.info
            if (!info?.id) break
            if (children.delete(info.id)) break
            if (session === info.id) reset()
            sendSync("session-end", { session_id: info.id })
            break
          }

          case "server.instance.disposed": {
            // The process is going away (TUI quit, `opencode run` done);
            // session.deleted only fires on an explicit delete.
            if (!session) break
            const sessionID = session
            reset()
            sendSync("session-end", { session_id: sessionID })
            break
          }
        }
      } catch {
        // never fail OpenCode
      }
    },
  }
}
