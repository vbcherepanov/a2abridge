# a2abridge

## Stop copy-pasting between your AI coding agents.

Claude in your IDE, Codex in a terminal, Cursor in another window —
all isolated by default. **a2abridge** wires them into a single
[A2A 1.0](https://a2a-protocol.org/latest/) mesh: peers find each
other, share an inbox, send each other tasks, and survive across
machines with mTLS. No vendor lock-in — runs on the open Linux
Foundation standard.

[![Build](https://github.com/vbcherepanov/a2abridge/actions/workflows/build.yml/badge.svg)](https://github.com/vbcherepanov/a2abridge/actions/workflows/build.yml)
[![Release](https://img.shields.io/github/v/release/vbcherepanov/a2abridge?display_name=tag&sort=semver)](https://github.com/vbcherepanov/a2abridge/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/vbcherepanov/a2abridge.svg)](https://pkg.go.dev/github.com/vbcherepanov/a2abridge)
[![Go Report Card](https://goreportcard.com/badge/github.com/vbcherepanov/a2abridge)](https://goreportcard.com/report/github.com/vbcherepanov/a2abridge)
[![A2A Protocol 1.0](https://img.shields.io/badge/A2A%20Protocol-1.0-1f6feb)](https://a2a-protocol.org/latest/specification/)
[![Linux Foundation](https://img.shields.io/badge/Linux%20Foundation-LF%20AI%20%26%20Data-0f1f3d)](https://lfaidata.foundation/)
[![Go 1.25](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev/)
[![macOS · Linux · Windows · WSL2](https://img.shields.io/badge/OS-macOS%20%7C%20Linux%20%7C%20Windows%20%7C%20WSL2-success)]()
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/badge/Sponsor-%F0%9F%92%9B-pink)](https://github.com/sponsors/vbcherepanov)

---

## The pain it solves

You have several AI coding agents on the same machine: Claude in the IDE, Claude in a terminal, Codex CLI, Cursor, maybe Cline or Gemini CLI. They are **isolated by default**. There is no built-in way for "Codex finished refactoring the API" to reach the Claude session that owns the frontend, except you copy-pasting between windows.

Existing solutions are either:

- **vendor-locked** — Anthropic Agent Teams talks Claude↔Claude only;
- **closed protocols** — CCB, claude-multi-agent-bridge, ruflo: each invents its own wire format, you cannot bring a third-party A2A agent in;
- **enterprise-only** — ruflo's federation is great but designed for 100+ agent swarms with central queens.

`a2abridge` takes a different angle: **wrap each agent into a standard [A2A 1.0](https://a2a-protocol.org/latest/specification/) peer**. Discovery is local; transport is plain JSON-RPC 2.0 + SSE; the wire is the open Linux Foundation spec. Any A2A-compliant agent (yours, third-party, future Google ADK, LangGraph, CrewAI, etc.) can join the same mesh on the same laptop.

## Why A2A and not a custom protocol

In August 2025 [IBM's ACP merged into Google's A2A](https://lfaidata.foundation/communityblog/2025/08/29/acp-joins-forces-with-a2a-under-the-linux-foundations-lf-ai-data/) under the Linux Foundation. By April 2026 there are 150+ supporting organizations and v1.2 is the current stable release; native A2A is in Google ADK, LangGraph, CrewAI, LlamaIndex Agents, Semantic Kernel, AutoGen. A2A won the protocol war. Your bridge should speak the protocol the rest of the industry speaks.

## Comparison

|                       | a2abridge | [Anthropic Agent Teams](https://code.claude.com/docs/en/agent-teams) | [CCB](https://github.com/bfly123/claude_codex_bridge) | [claude-multi-agent-bridge](https://github.com/yakub268/claude-multi-agent-bridge) | [ruflo](https://github.com/ruvnet/ruflo) |
|---|---|---|---|---|---|
| **Open protocol** | A2A 1.0 (LF) | Closed | Closed | Closed | Closed |
| **Cross-vendor agents** | Any A2A peer | Claude only | Claude/Codex/Gemini/Droid | Claude only | Claude (via plugins) |
| **Transport** | JSON-RPC 2.0 + SSE over HTTP | Internal mailbox | Unix sockets + tmux | Flask HTTP + SSE + SQLite | WebSocket + mTLS |
| **Discovery** | Local directory + Agent Card | Built-in lead/teammate | Project `.ccb/` registry | Single Flask server | Federation registry |
| **Cross-machine** | Yes (mTLS+ed25519, opt-in) | No | No | No | Yes (zero-trust) |
| **Push notifications** | Yes (A2A 1.0 §9.5 webhooks) | Idle hooks | Polling | SSE | WebSocket |
| **Install footprint** | Single Go binary, `~10 MB` | Built into Claude Code | Python 3.10+ + tmux | Python + Flask + Chrome ext | npm/Node + 32 plugins |
| **Cross-platform install** | macOS / Linux / Windows / WSL2 (one cmd) | macOS-leaning (tmux) | install.sh + install.ps1 | Python | npm |
| **Lifecycle** | Per-agent bridge dies with MCP stdio session — no orphans | Lead-managed | Daemon `ccbd` per project | Single shared server | Distributed |
| **MCP stdio for IDE** | Yes (any MCP client) | N/A (native) | Yes (delegation registry) | Yes (Claude Desktop) | Yes |
| **Production state** | v0.x (this rewrite) | Experimental flag | v6.x | v1.x | v0.5+ |

`a2abridge` is **not** trying to be ruflo (we are not building a 100-agent federated swarm) and **not** Agent Teams (we are not a built-in Claude feature). The niche is exactly: "cross-vendor, open-protocol, single-laptop mesh that any new A2A agent can drop into."

## What you get

1. **Each running agent is an A2A peer.** Agent Card on `/.well-known/a2a`, full JSON-RPC 2.0 binding (`SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`, `GetExtendedAgentCard`), TaskState/Message/Part/Artifact, error codes per spec §8, header `A2A-Version: 1.0`.
2. **Local directory** for zero-config discovery on your machine. Run as a system service (launchd / systemd-user / Windows Service) — same UX on every OS.
3. **MCP tools** plugged into your IDE: `a2a_whoami`, `a2a_list_agents`, `a2a_send_message`, `a2a_send_streaming`, `a2a_get_task`, `a2a_cancel_task`, `a2a_inbox`, `a2a_complete_task`.
4. **One-line install** that detects every supported IDE on your machine and registers the MCP server with `.bak` backups of your configs.
5. **Skill `a2a-bridge`** for Claude Code that loads only when relevant — no globally-loaded rules eating tokens on every session.

## Use cases — what this actually unlocks

### 1. Cross-stack contract changes propagate without copy-paste

You change a JSON shape in `backend/api.go`. Your Claude Code session
running there fires off:

```
a2a_send_message peer_url=<frontend-claude> text="[backend-3a2f] FYI:
GET /orders now returns `currency` (string, ISO 4217). Was implicit USD."
```

The frontend Claude session sees that line **at the top of your next
prompt** (via the UserPromptSubmit hook), so before it touches
`useOrders.ts` it already knows the field is required. No Slack ping,
no PR review delay.

### 2. One refactor, several agents fan out the work

```
You (in Claude Code, IDE):
  Refactor the payment module. Delegate the Go service to Codex,
  the Vue checkout to the Cursor session, and have the Cline session
  audit the migration once Go finishes.

Claude:
  → a2a_send_message peer_url=<codex> text="..."     # Go service
  → a2a_send_message peer_url=<cursor> text="..."    # Vue checkout
  Tracks both task IDs; when Codex completes, fires off the
  Cline audit task with Codex's diff attached.
```

Three agents working concurrently, one human in the loop.

### 3. An always-online agent picks up tasks while you sleep

```
$ a2abridge worker start --cmd claude --prompt "You are a maintenance
  agent. Drain a2a_inbox and address every request. If unsure, reply
  with a question and stop."
```

Now any peer (yours or a teammate's) can `a2a_send_message` to that
worker URL and get a reply hours later — even after you've closed every
IDE window. The worker survives reboots if you `a2abridge service
install` the directory daemon.

### 4. ADK / LangGraph / CrewAI agents drop into the same mesh

a2abridge is the **open-protocol** bridge — third-party agents that
speak A2A 1.0 are first-class peers without glue code. See
[`docs/integrations/`](docs/integrations/) for working examples with
Google ADK 1.0, LangGraph 0.4.7+, CrewAI 0.95+ and LlamaIndex 0.13+.

### 5. Cross-machine federation (laptop ↔ desktop ↔ remote dev box)

Set `A2A_TLS_CERT` / `A2A_TLS_KEY` / `A2A_TRUST_ROOTS` on each side,
optionally `A2A_MDNS=1` for LAN auto-discovery, and your home Mac talks
to your office Linux box over mTLS-authenticated A2A. ed25519 cert
generation is one command (`a2abridge cert generate`). See [Security
model](#security-model) below.

## Quick start

### macOS / Linux / WSL2

```bash
curl -fsSL https://raw.githubusercontent.com/<owner>/a2abridge/main/install.sh | bash
```

### Windows (PowerShell as user, no admin)

```powershell
iwr -useb https://raw.githubusercontent.com/<owner>/a2abridge/main/install.ps1 | iex
```

### What the installer does

1. Downloads the right binary from GitHub Releases for your `os/arch`.
2. Drops it into `~/.a2abridge/bin/a2abridge` and `chmod +x`.
3. Detects installed IDEs and writes the MCP block to each one's config — with timestamped `.bak` next to the original:
   - Claude Code → `~/.claude/settings.json` (or `~/.claude.json` if present)
   - Codex CLI → `~/.codex/config.toml`
   - Cline (VS Code) → `~/.config/Code/User/settings.json` (`cline.mcpServers`)
   - Continue → `~/.continue/config.json`
   - Cursor → `~/.cursor/mcp.json`
   - Gemini CLI → `~/.gemini/settings.json`
4. Registers `a2abridge directory` as a user-level system service (`launchd` / `systemd --user` / Windows Service) and starts it on `127.0.0.1:7777`.
5. Installs the `a2a-bridge` skill into `~/.claude/skills/a2a-bridge/` and adds `~/.claude/hooks/a2a-inbox-hook.sh` (so other agents' messages show up in your prompt).
6. Runs `a2abridge doctor` to verify everything is healthy.

To preview without making changes: `a2abridge install --dry-run`.

## First run — Hello World between two agents

After the installer completes, here's the 3-step verification that
everything is wired correctly. Two terminal windows are enough.

**Terminal 1 — open Claude Code in any project**

```bash
cd ~/some/project
claude    # or just open VS Code with the Claude Code extension
```

In the chat, type:

```
What's my A2A label and which peers are online?
```

Claude will call `a2a_whoami` (returns its own Agent Card with a stable
label like `claude-ttys000`) and `a2a_list_agents` (returns the list of
peers — empty for now since you only have one bridge running).

**Terminal 2 — open Codex CLI in another project**

```bash
cd ~/another/project
codex
```

Type:

```
List the A2A peers you can see.
```

Codex calls `a2a_list_agents` — and now sees the Claude bridge from
Terminal 1. Send it a message:

```
Send "ping from codex" to the claude-ttys000 peer.
```

Codex calls `a2a_send_message`. The message is queued in Claude's inbox.

**Terminal 1 — type any prompt**

The UserPromptSubmit hook drains the inbox automatically and prepends
the message above your prompt:

```
You have 1 unread A2A message(s):
- from `codex-ttysXXX` (task ABC...): ping from codex
```

Claude can now `a2a_complete_task task_id=ABC text="pong"` and Codex
will see the reply on its next turn (the SSE fast-path delivers it in
~milliseconds).

**Verify directly via curl** (always works, regardless of IDE state):

```bash
curl http://127.0.0.1:7777/agents | jq .
# → list of registered peers and their URLs

PEER_URL=$(curl -s http://127.0.0.1:7777/agents | jq -r '.[0].url')
curl "$PEER_URL/.well-known/a2a" | jq .
# → that peer's Agent Card
```

If anything looks wrong, run `a2abridge doctor` — it tells you exactly
which check failed and how to fix it.

## Per-agent setup notes

The `a2abridge install` step writes the right MCP block into each
detected IDE's config. This section explains what that block looks like
per IDE so you can verify by hand or wire it up manually.

### Claude Code (`~/.claude/settings.json`)

```json
{
  "mcpServers": {
    "a2a": {
      "command": "/Users/<you>/.a2abridge/bin/a2abridge",
      "args": ["bridge"],
      "env": {
        "A2A_DIRECTORY": "http://127.0.0.1:7777",
        "A2A_BIND": "127.0.0.1:0",
        "A2A_NAME": "claude-ide",
        "A2A_MODEL": "claude-opus-4-7",
        "A2A_SKILLS": "go,php,vue,refactor,review"
      }
    }
  },
  "hooks": {
    "UserPromptSubmit": [{
      "matcher": "*",
      "hooks": [{
        "type": "command",
        "command": "/Users/<you>/.claude/hooks/a2a-inbox-hook.sh"
      }]
    }]
  }
}
```

The hook script auto-injects the inbox before every prompt. Restart
Claude Code after editing.

### Codex CLI (`~/.codex/config.toml`)

```toml
[mcp_servers.a2a]
command = "/Users/<you>/.a2abridge/bin/a2abridge"
args = ["bridge"]

[mcp_servers.a2a.env]
A2A_DIRECTORY = "http://127.0.0.1:7777"
A2A_BIND = "127.0.0.1:0"
A2A_NAME = "codex"
A2A_SKILLS = "code,plan"
```

Verify with `codex mcp list` — `a2a` should be listed as `enabled / stdio`.

### Cursor (`~/.cursor/mcp.json`)

Same shape as Claude Code's `mcpServers` block. Restart Cursor.

### Cline (VS Code)

Cline reads MCP from
`<vs-code-globalStorage>/saoudrizwan.claude-dev/settings/cline_mcp_settings.json`,
not VS Code's main `settings.json`. The installer drops the block there
automatically.

### Continue

Continue 1.x reads MCP servers from
`~/.continue/mcpServers/<name>.yaml`. The installer writes
`a2a.yaml` there with the equivalent fields.

### Gemini CLI (`~/.gemini/settings.json`)

Same `mcpServers` shape as Claude Code. After a restart, `gemini /mcp`
will list the `a2a` server.

### Two instances of the same IDE on the same host

Override `A2A_ID` and `A2A_NAME` per-window so they don't fight for the
same directory entry:

```bash
A2A_ID=claude-ide-main A2A_NAME=claude-ide claude  # window 1
A2A_ID=claude-term     A2A_NAME=claude-term claude # window 2
```

Without overrides, the second window will quietly overwrite the first
in the directory.

## Manual install (no curl)

```bash
git clone https://github.com/<owner>/a2abridge ~/PROJECT/a2abridge
cd ~/PROJECT/a2abridge
go build -o ~/.a2abridge/bin/a2abridge ./cmd/a2abridge
~/.a2abridge/bin/a2abridge install --ide auto
```

## Architecture

```
                            ┌─────────────────────────────┐
                            │   a2abridge directory       │  user-level system service
                            │   :7777 (loopback)          │  (launchd · systemd-user · WinSvc)
                            │   POST /register            │
                            │   GET  /agents              │
                            └────────▲────────────────────┘
                                     │ heartbeat / advertise own URL
              ┌──────────────────────┼──────────────────────┐
              │                      │                      │
   ┌──────────┴──────────┐ ┌─────────┴───────────┐ ┌────────┴───────────┐
   │ Claude Code (IDE)   │ │ Claude Code (term)  │ │ Codex CLI / Cursor │
   │ ─ MCP stdio         │ │ ─ MCP stdio         │ │ ─ MCP stdio        │
   │     ↕               │ │     ↕               │ │     ↕              │
   │ a2abridge bridge    │ │ a2abridge bridge    │ │ a2abridge bridge   │
   │ http://127.0.0.1:N  │ │ http://127.0.0.1:M  │ │ http://127.0.0.1:K │
   │ A2A server + client │ │ A2A server + client │ │ A2A server + client│
   └──────────┬──────────┘ └─────────┬───────────┘ └────────┬───────────┘
              │                      │                      │
              └──────────  JSON-RPC 2.0 + SSE  ──────────────┘
                       (peers talk to peers, directly)
```

- One binary, multiple subcommands. `a2abridge directory` runs as a daemon. `a2abridge bridge` is launched as MCP stdio server by your IDE — its lifecycle equals your IDE session.
- Per-agent state lives in `./.a2a/` inside your current working directory (inbox, label, logs). Out-of-cwd state lives under `~/.a2abridge/`.
- All loopback by default. Cross-machine federation lands in Phase 2 (mTLS + ed25519, opt-in).

## Subcommands

| Command | What |
|---|---|
| `a2abridge install [--ide auto\|claude-code,codex,...] [--dry-run] [--apply]` | Detect IDEs, write MCP configs (`.bak` backups), install service, install skill, register UserPromptSubmit hook |
| `a2abridge directory [--addr 127.0.0.1:7777]` | Run discovery service (used by the system service unit) |
| `a2abridge bridge` | Run as MCP stdio server (used by IDEs, do not call manually) |
| `a2abridge service {install\|start\|stop\|restart\|status\|uninstall}` | Manage the directory daemon under launchd / systemd-user / Windows Service |
| `a2abridge doctor` | Health check: directory ping, ports, IDE configs, skill, hook, version |
| `a2abridge update [--check]` | Self-update from the latest GitHub release with rollback on failure |
| `a2abridge uninstall [--purge] [--keep-service]` | Remove the MCP block from every IDE config, skill, hook, and service (`.bak` unless `--purge`) |
| `a2abridge cert generate [--cn <name>] [--dir <path>]` | Produce an ed25519 self-signed cert + key for federation |
| `a2abridge completion {bash\|zsh\|fish\|powershell}` | Emit a tab-completion script for your shell |

## MCP tools

| Tool | Maps to | Use |
|---|---|---|
| `a2a_whoami` | own Agent Card | "who am I to other agents" |
| `a2a_list_agents` | `GET /agents` on directory | list peers + their cards |
| `a2a_send_message` | `a2a.SendMessage` | fire-and-forget or blocking task |
| `a2a_send_streaming` | `a2a.SendStreamingMessage` | wait for completion via SSE |
| `a2a_get_task` | `a2a.GetTask` | poll a previously sent task |
| `a2a_cancel_task` | `a2a.CancelTask` | cancel a running task |
| `a2a_inbox` | local inbox | fetch unread incoming messages |
| `a2a_complete_task` | `a2a.SendMessage` reply + state→COMPLETED | answer an incoming task |

## Skill (Claude Code)

The installer drops a skill into `~/.claude/skills/a2a-bridge/`. It auto-loads when your prompt contains triggers like *"a2a"*, *"another agent"*, *"smoke test peers"*, *"FYI to backend"*, *"broadcast"*. The skill teaches Claude:

- the inbound flow (poll inbox before answering you, classify FYI vs request-for-action),
- when to send proactive FYI to peers (API contract changes, schema changes, infra changes),
- the self-label format `<basename-cwd>-<4hex>` so peers know who is talking,
- how to use each MCP tool with real examples.

Without the skill, Claude has the tools but no idea when to use them. With the skill, you do not need to ask "check inbox" — it does it on its own.

## Configuration

Set per-bridge environment in your IDE config (the installer fills sensible defaults):

| Env | Default | Meaning |
|---|---|---|
| `A2A_DIRECTORY` | `http://127.0.0.1:7777` | URL of the directory service |
| `A2A_BIND` | `127.0.0.1:0` | bind addr/port (`:0` = random free port) |
| `A2A_ADVERTISE_HOST` | `127.0.0.1` | host to advertise to the directory |
| `A2A_NAME` | derived from binary path | human-readable name shown to peers |
| `A2A_ID` | `<name>-<pid>` | stable id for the peer |
| `A2A_MODEL` | unset | model id reported in Agent Card (`claude-opus-4-7`, `gpt-5`, ...) |
| `A2A_SKILLS` | unset | comma-separated capability tags |
| `A2A_STATE_DIR` | `./.a2a` | per-project inbox/label/log directory |

Cross-machine (Phase 2): `A2A_TLS_CERT`, `A2A_TLS_KEY`, `A2A_TRUST_ROOTS`, `A2A_PEER_ALLOW`.

## How the inbox flow actually works

The hardest thing to internalise is **when** peers see messages. Here's
the model in 5 lines:

1. Peer A calls `a2a_send_message peer_url=<B>` — task lands in B's
   `Store` and B's `inbox` file (`./.a2a/inbox-<ppid>.json`).
2. B's UserPromptSubmit hook reads the inbox **on B's next prompt**
   from the human user — and prepends it to the system prompt.
3. B answers the human. Within that turn (or the next) B can call
   `a2a_complete_task task_id=...` to reply to A.
4. The reply lands as a `COMPLETED` Task on A's bridge. The bridge's
   SSE subscriber sees it instantly (or, fallback, the 5-second poller).
5. A gets a synthetic message in **its** inbox: "ОТВЕТ от B на твой
   вопрос «...»" — which prints on A's next prompt the same way.

So end-to-end latency is **one prompt from the recipient + one from
the sender** to see the reply. That is the same delay you'd have over
DM with a colleague — the bridge doesn't wake idle agents, it just
makes sure neither side misses the message.

## Troubleshooting

```bash
a2abridge doctor
```

Output is a table of checks, each PASS/WARN/FAIL with a fix hint. Typical failures:

- **directory not running** → `a2abridge service start`
- **port 7777 in use** → `a2abridge service uninstall && a2abridge install --directory-port 7778`
- **IDE config missing the MCP block** → re-run `a2abridge install --ide claude-code`
- **inbox stale** → `rm -rf ./.a2a/inbox` (the bridge will rebuild on next message)
- **two Claude windows clobbering each other in the directory** → set distinct `A2A_ID` env per window

Logs:

- macOS: `~/Library/Logs/a2abridge/directory.log`
- Linux: `journalctl --user -u a2abridge-directory.service`
- Windows: `Get-EventLog -LogName Application -Source a2abridge-directory`
- Per-bridge: `./.a2a/bridge.log`

## Compatibility

| Platform | Service supervisor | Tested |
|---|---|---|
| macOS 13+ (Intel and Apple Silicon) | launchd | yes |
| Ubuntu 22.04+ / Debian 12+ / Fedora 40+ | systemd --user | yes |
| Arch / openSUSE / RHEL 9+ | systemd --user | best-effort |
| Windows 11 | Windows Service Manager | yes |
| WSL2 (Ubuntu) with `systemd=true` | systemd --user inside WSL | yes |
| WSL2 without systemd | Windows-side service + `wsl.exe` shim | yes |

| AI agent | MCP integration | Verified |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | yes |
| Codex CLI | `~/.codex/config.toml` | yes |
| Cline (VS Code) | `cline.mcpServers` in VS Code settings | yes |
| Continue | `~/.continue/config.json` | yes |
| Cursor | `~/.cursor/mcp.json` | yes |
| Gemini CLI | `~/.gemini/settings.json` | yes |
| Aider | `aider.conf.yml` (bridge mode) | best-effort |
| Any A2A 1.0 peer | direct JSON-RPC | yes |

## Security model

- **Loopback by default.** Directory and bridges bind `127.0.0.1`. Other users on the machine cannot see your peers.
- **PII / secret screen** before `a2a_send_message`: 11 regex detectors (AWS access key, GitHub PAT, Anthropic / OpenAI / Google / Stripe / Slack tokens, JWT, PEM private keys, ...). Matches are replaced with `[REDACTED:<name>]` and surfaced in MCP metadata. The message still goes through with redacted text — the secret never leaves the bridge.
- **mTLS + ed25519 federation** (opt-in cross-machine): set `A2A_TLS_CERT`, `A2A_TLS_KEY`, `A2A_TRUST_ROOTS`, `A2A_PEER_ALLOW` and the bridge serves over TLS 1.3 with required client certs and an allow-list match on peer CN/SAN. Keys are generated by `a2abridge cert generate`.
- **User hook scripts**: drop `~/.a2abridge/hooks/{on-inbound,on-outgoing-reply}.sh` (or `.ps1`/`.cmd` on Windows) and the bridge fires them on every inbound message and outbound reply (5-second timeout, JSON on stdin, fields in `A2A_EVENT_*` env vars).
- **No telemetry.** Period.

## Roadmap

### v1.0.0 — shipped
- [x] Single binary with subcommands (directory, bridge, install, uninstall, update, service, doctor, cert, completion)
- [x] `kardianos/service` cross-platform supervisor (launchd · systemd-user · Windows Service · WSL2)
- [x] `a2abridge install` with auto-detection of 6 IDEs and `.bak` backups
- [x] `install.sh` + `install.ps1` one-line installers
- [x] `a2abridge doctor` — 9-check health audit
- [x] Project-local `./.a2a/` state with `~/.a2abridge/state/<ppid>` fallback
- [x] Skill `a2a-bridge` + UserPromptSubmit hook shipped via installer
- [x] SSE fast-path for outbound replies (`a2a.SubscribeToTask`) with polling fallback
- [x] PII / secret screen before send (11 regex detectors)
- [x] User hook scripts on `on-inbound` / `on-outgoing-reply`
- [x] Push Notifications per A2A 1.0 §9.5 (4 RPC methods + webhook delivery)
- [x] HTTP+REST binding per A2A 1.0 §7.3
- [x] mTLS + ed25519 federation (opt-in cross-machine)
- [x] `a2abridge cert generate` for ed25519 self-signed cert/key
- [x] Self-update + uninstall + shell completion
- [x] 35 test cases under `-race`, GitHub Actions test + release matrix

### v1.1.0 — shipped
- [x] mDNS / DNS-SD cross-machine discovery (`A2A_MDNS=1`)
- [x] `a2abridge service install --federation` one-step cert + service install
- [x] Push notification retry with exponential backoff (5xx + network errors)

### v2.0.0 — shipped
- [x] `a2abridge worker {start|stop|status|attach}` — always-online Claude in detached tmux
- [x] Integration docs for Google ADK 1.0, LangGraph, CrewAI, LlamaIndex Agents — see [docs/integrations/](docs/integrations/)

### v2.1 — next (open)
- [ ] gRPC binding per A2A 1.0 §7.2 (deferred — needs `protoc` toolchain in CI)
- [ ] Enterprise SSO / SCIM for the directory in multi-team installs
- [ ] Signed-manifest WAN discovery (alternative to mDNS for cross-internet peers)

## FAQ

**Does this require an Anthropic / OpenAI API key?**
No. Each peer is whatever LLM your IDE is already using on its own
subscription. a2abridge is just the wire between them — it never calls
an LLM API itself.

**How many tokens does this burn?**
Zero on its own. The bridge does HTTP, not LLM calls. Your IDE pays for
the prompts it generates when it decides to use `a2a_*` tools — same
billing as any other tool call.

**What if I already have other MCP servers in my Claude config?**
The installer merges the `a2a` entry into the existing `mcpServers`
map without disturbing anything else, and creates a `.bak.<timestamp>`
of the original file. Run `a2abridge install --dry-run` to preview.

**Does it work over a corporate VPN / behind a proxy?**
Single-machine setup is loopback-only — no proxy involved. Cross-machine
mode (mTLS) goes over plain HTTPS, so any proxy that lets you reach a
peer's IP and port works. mDNS won't traverse most VPNs — use the
directory daemon over an internet-reachable IP instead.

**Does it leak my code / messages anywhere?**
No telemetry, no third-party calls. Default loopback bind is `127.0.0.1`
— other users on the machine cannot see your peers. Outbound messages
pass through an 11-pattern PII screen that strips AWS / GitHub / API
keys / JWTs / PEM blocks before they leave the bridge.

**Can I disable the auto-injecting hook?**
Yes. Either delete `~/.claude/hooks/a2a-inbox-hook.sh`, or remove the
`hooks.UserPromptSubmit` block from `~/.claude/settings.json`, or run
`a2abridge uninstall --keep-service` (preserves the directory daemon
but rolls back the hook + skill + IDE configs).

**Why is my second Claude window invisible to peers?**
Both windows share the same default `A2A_ID`, so the second one
overwrites the first in the directory. Set distinct `A2A_ID` /
`A2A_NAME` per window — see [Per-agent setup notes](#per-agent-setup-notes).

**Does Cline / Continue / Cursor really work?**
Yes — they all support MCP stdio servers with the same `mcpServers`
shape. The installer writes the right path for each one. Run
`a2abridge doctor` to confirm.

**What happens if the directory daemon crashes?**
Bridges keep running; they just lose discovery for new peers. Existing
`peer_url` references still work (they're cached in the inbox / task
metadata). When the daemon comes back, bridges auto-re-register on
their next 30-second heartbeat. If you're running cross-machine with
mDNS, peers keep finding each other directly via DNS-SD even with the
daemon down.

**Why JSON-RPC and not gRPC?**
JSON-RPC works in every browser fetch / cURL script and needs no
protoc toolchain. gRPC binding (A2A 1.0 §7.2) is on the roadmap for
v2.1 — open issue if you'd use it, otherwise we skip it.

## Contributing

Issues and PRs welcome. The code base is small (~2k LOC of Go) and the spec is fixed, so contributions are easy to scope.

```bash
git clone https://github.com/<owner>/a2abridge
cd a2abridge
go test ./...
go build ./cmd/a2abridge
./a2abridge doctor
```

Conventional Commits, English subjects, no AI co-authors.

## Specification compliance

`a2abridge` is implemented against [A2A Protocol Specification 1.0](https://a2a-protocol.org/latest/specification/):

- Agent Card (§5) on `/.well-known/a2a`
- JSON-RPC 2.0 binding (§7) — all eight methods
- TaskState enum (§6.4): `SUBMITTED`, `WORKING`, `COMPLETED`, `FAILED`, `CANCELED`, `INPUT_REQUIRED`, `REJECTED`, `AUTH_REQUIRED`
- Message / Part / Artifact (§6.1–6.6) with `ROLE_USER` / `ROLE_AGENT`
- Stream one-of: `task | message | statusUpdate | artifactUpdate`
- Error codes from §8 (`-32001 TaskNotFound`, `-32003 PushNotificationNotSupported`, `-32004 UnsupportedOperation`, ...)
- Header `A2A-Version: 1.0`
- Push Notifications (§9.5) — `CreateTaskPushNotificationConfig`, `Get`, `List`, `Delete` plus webhook delivery on every state change
- HTTP+REST binding (§7.3) mirroring all RPC verbs at `/v1/...`
- mTLS server/client auth with ed25519 cert generation

Not yet implemented (v1.1+): gRPC binding (§7.2), cross-machine directory discovery via mDNS, retry policy for push delivery.

## Support the project

a2abridge is MIT-licensed and free, but it's also a one-developer
project that runs on weekends. If it saves you copy-paste between
agents, consider chipping in:

- 💛 [GitHub Sponsors](https://github.com/sponsors/vbcherepanov) — recurring or one-off, GitHub-native
- 💸 [PayPal](https://PayPal.Me/vbcherepanov) — one-shot donations

Sponsors get their name in the repo `README` and direct line for
feature requests.

## License

MIT. See [LICENSE](LICENSE).

## Credits

Built on the open work of:
- [Linux Foundation A2A project](https://github.com/a2aproject/A2A) — protocol specification
- [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) — MCP server SDK
- [`kardianos/service`](https://github.com/kardianos/service) — cross-platform service supervisor
- [`google/uuid`](https://github.com/google/uuid)
