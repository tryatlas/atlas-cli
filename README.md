# atlas-cli

A headless Atlas for servers: a Go binary with a Bubble Tea TUI that signs in
like the Atlas desktop app, binds a folder to an organisation and a cloud
Project, and gives every coding agent run in that folder **shared memory** and
**session capture**, byte-compatible with the desktop's on-disk formats.

```
atlas login            # device grant against the Atlas auth server
cd ~/work/backend
atlas init             # org + cloud Project, writes .atlas/
atlas memory enable
atlas capture enable
atlas run codex        # the product: any registry agent over ACP, chat TUI,
atlas run claude-code  #   full capture, cloud sync, shared memory over the wire
atlas status
```

## Where things are

| path | what |
|---|---|
| `docs/atlas-reference/README.md` | index of the hand-off folder copied from the desktop repo: read first |
| `docs/atlas-reference/SPEC.md` | the specification; §1.2 holds the locked decisions D1–D20 |
| `docs/atlas-reference/evidence-*.md` | desktop-side facts with `file:line` citations into the desktop repo |
| `docs/atlas-reference/adr/` | the desktop ADRs this project leans on |
| `docs/adr/` | this repo's own ADRs; `0000` records D7, `0001`–`0007` record D13–D19, `0009` records D20 (ACP only, the app's way); `0000`, `0003`, `0004` are superseded |

Ground rules that are not negotiable: never read another agent's transcript
files, never inject memory into prompts, never log an `Authorization` header,
every stored text passes the redactor first, and no secret persists except the
session token file (mode 0600).
