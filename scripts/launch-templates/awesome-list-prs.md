# Submissions to awesome-* lists

Each list has its own PR template. Below are the entries to paste into
each one's README, plus a per-list note about format.

## awesome-claude-code

Repo: <https://github.com/hesreallyhim/awesome-claude-code>

PR target: README.md, under the `### Tooling` section (or `### Plugins`
if MCP-server-shaped).

```md
- [a2abridge](https://github.com/vbcherepanov/a2abridge) — Open A2A 1.0
  bridge that lets multiple Claude Code sessions (and Codex / Cursor /
  Cline / Continue / Gemini sessions) discover each other and exchange
  tasks. Single Go binary, mTLS for cross-machine, 44 tests, MIT.
```

## awesome-mcp

Repo: <https://github.com/punkpeye/awesome-mcp-servers>

PR target: README.md, under `## Server Implementations`.

```md
- [a2abridge](https://github.com/vbcherepanov/a2abridge) — MCP server
  that exposes Agent-to-Agent (A2A 1.0) tools so AI coding agents on the
  same machine can discover, message and delegate to each other. Works
  with Claude Code, Codex CLI, Cursor, Cline, Continue, Gemini CLI.
```

## awesome-go

Repo: <https://github.com/avelino/awesome-go>

PR target: README.md, under `## Distributed Systems`. **Strict
requirements**: must be tagged with semver, must have `go.mod`, must
have ≥3 stars. We'll have all three by the time we submit.

```md
- [a2abridge](https://github.com/vbcherepanov/a2abridge) - A2A protocol
  bridge that turns AI coding agents into discoverable peers, with
  JSON-RPC, REST, SSE, Push Notifications and mTLS federation.
```

## awesome-a2a (if it exists by submission time)

If <https://github.com/a2aproject/awesome-a2a> appears, target the
"Bridges & Adapters" or "Implementations" section:

```md
- [a2abridge](https://github.com/vbcherepanov/a2abridge) — Local-first
  A2A 1.0 bridge for AI coding agents (Claude Code, Codex, Cursor,
  Cline, Continue, Gemini). Implements §5 Agent Card, §7 JSON-RPC + REST,
  §9.5 Push Notifications. Cross-machine via mTLS + ed25519.
```

## A2A Protocol partners page

Submit via the form at <https://a2a-protocol.org/community/> — they
maintain a curated implementations list. They want:

- Project name
- One-line description
- Repo URL
- Implementation language
- A2A version supported
- Logo (optional, 256×256 PNG)

Drop `vbcherepanov/a2abridge`, "Open A2A 1.0 bridge for AI coding
agents", `Go`, `1.0` and skip the logo (or generate later).
