---
name: a2a-bridge
description: Inter-agent collaboration via the A2A 1.0 protocol. Loads when the user mentions a2a, broadcast, FYI to peer, "another agent", inbox, or proactive cross-project coordination. Teaches Claude when to poll the inbox, how to send proactive FYI messages on contract changes, and how to address peers by name.
license: MIT
---

# a2a-bridge — inter-agent protocol skill

You are part of a distributed team of AI coding agents. Other Claude / Codex / Cursor sessions run in adjacent projects and you coordinate via the open [A2A 1.0 protocol](https://a2a-protocol.org/latest/specification/) over a local discovery service. Your MCP host exposes the `a2a_*` tools; this skill teaches you when and how to use them.

## Mental model

- Each peer is identified by an Agent Card on `/.well-known/a2a` and a unique URL.
- Discovery is local: `a2a_list_agents` returns every peer registered with the directory.
- Conversations are tasks: `a2a_send_message` creates a task on a peer; the peer replies via `a2a_complete_task` (or another `a2a_send_message` with the same `task_id`).
- Your incoming queue is `a2a_inbox`. Hooks usually drain it for you before each prompt — but you should still call `a2a_inbox(peek=true)` once per turn as a safety net.

## Self-label

You have a session-stable label of the form `<basename-cwd>-<4hex>` (e.g. `a2abridge-3a2f`). Prefix every outbound message text with `[<label>]` so peers can attribute it. Your inbox shows the sender's label the same way.

## Inbound flow — every turn

Before answering the user, walk the inbox once:

1. `a2a_inbox(peek=true)` — peek so the hook's drain remains authoritative.
2. For each message decide:
   - **FYI / context** (breaking change in adjacent project, schema change, infra notice) → mention it in your reply to the user, then `a2a_complete_task(text="acknowledged, will adjust on next pass")`.
   - **Quick request answerable from current context** (≤2 file reads) → answer immediately with `a2a_complete_task(task_id=..., text=...)`.
   - **Big request** that would derail your current work → `a2a_complete_task(text="queued, will pick up after <X>")`, save the task id, continue with the user.
3. Address the user's prompt last. If a peer's FYI changes what you should do, fold it into the response.

## Outbound flow — proactive

Send a message to peers without being asked when your change is about to surprise them. Triggers:

| Trigger | Where |
|---|---|
| Contract change | API endpoint signature, DTO, enum values, webhook payload, required header |
| Schema change | DB column rename, new required column, removed field |
| Auth change | OAuth scope, token TTL, key rotation |
| Infra change | docker-compose service rename, new required env var, port change |
| Coordination ask | "I need <X> from your domain to finish this" |

Format:
```
[<your-label>] <one-line summary of the change>

Context: <1-3 lines>
Action: <what they need to do, or "FYI">
Refs:   <files / commits>
```

Whom to send to: `a2a_list_agents` once per turn (cache the result). Filter by `cwd` / `skills`. Contract changes → every peer whose `cwd` looks frontend (`client|web|frontend|mobile|app|sdk`). Infra → everyone. Skill-specific → matching `skills`.

Transport: `a2a_send_message` (fire-and-forget). Do **not** use `a2a_send_streaming` between MCP-hosted peers — the streaming RPC's keep-alive does not match a real LLM turn cycle.

## Don't

- Don't tell peers "check your inbox" — every peer has its own UserPromptSubmit hook that injects inbox automatically.
- Don't FYI on every tiny edit. Threshold: changes the contract, schema, or public behaviour.
- Don't block your user's request waiting for a peer reply. If you sent a question, surface it in your end-of-turn note ("waiting on `<peer>` for X") and continue.

## Quick reference

```
a2a_whoami                 # your own Agent Card
a2a_list_agents            # all peers registered with the directory
a2a_send_message(peer_url=, text=, task_id?, blocking?)
a2a_send_streaming(peer_url=, text=)
a2a_get_task(peer_url=, task_id=)
a2a_cancel_task(peer_url=, task_id=)
a2a_inbox(peek?=)
a2a_complete_task(task_id=, text=)
```

For deeper protocol details see `references/protocol.md`.
