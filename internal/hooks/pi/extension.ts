// Atlas extension for Pi — written by atlas agents add; do not edit (rewritten on the next add).
//
// Forwards Pi's session lifecycle to `atlas hooks pi <verb>` with the event
// as JSON on stdin (ADR-0013). A machine without atlas on PATH runs a silent
// no-op; a hook that fails or times out never blocks Pi.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";

const HOOK_TIMEOUT_MS = 10_000;
const MAX_OUTPUT = 4 * 1024 * 1024;

// A nested pi (a subagent spawns one per task, inside the project, where Pi
// finds this same extension) must not report its lifecycle as the user's
// session. The marker is inherited by every child of this process, including
// the atlas hook itself, so only the extension reads it.
const NESTED_ENV = "ATLAS_PI_NESTED";

function hookCommand(verb: string): string {
  return `if ! command -v atlas >/dev/null 2>&1; then exit 0; fi; exec atlas hooks pi ${verb}`;
}

function fire(verb: string, payload: Record<string, unknown>): Promise<void> {
  return new Promise((resolve) => {
    try {
      const child = execFile(
        "sh",
        ["-c", hookCommand(verb)],
        { timeout: HOOK_TIMEOUT_MS, windowsHide: true, maxBuffer: MAX_OUTPUT },
        () => resolve(),
      );
      child.stdin?.on("error", () => {});
      child.stdin?.end(JSON.stringify(payload));
    } catch {
      resolve();
    }
  });
}

export default function (pi: ExtensionAPI) {
  const nested = Boolean(process.env[NESTED_ENV]);
  process.env[NESTED_ENV] = "1";
  if (nested) return;

  pi.on("session_start", async (_event, ctx) => {
    await fire("session-start", {
      type: "session_start",
      cwd: ctx.cwd,
      session_file: ctx.sessionManager.getSessionFile(),
    });
  });

  pi.on("before_agent_start", async (event, ctx) => {
    await fire("before-agent-start", {
      type: "before_agent_start",
      cwd: ctx.cwd,
      session_file: ctx.sessionManager.getSessionFile(),
      prompt: event.prompt,
    });
  });

  pi.on("agent_end", async (_event, ctx) => {
    await fire("agent-end", {
      type: "agent_end",
      cwd: ctx.cwd,
      session_file: ctx.sessionManager.getSessionFile(),
    });
  });

  // session_shutdown is deliberately not forwarded: Pi fires it alongside
  // agent_end and the two hook processes race, so a session end could land
  // before the last turn is saved. The dispatcher ends idle sessions itself.
}
