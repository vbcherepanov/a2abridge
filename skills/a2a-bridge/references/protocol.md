# A2A 1.0 — protocol cheatsheet

a2abridge speaks the [Agent2Agent 1.0 specification](https://a2a-protocol.org/latest/specification/) under the Linux Foundation. This file is a quick reference; the spec is authoritative.

## Transport

JSON-RPC 2.0 over HTTP, with `Content-Type: application/json` and the header `A2A-Version: 1.0`. Streaming methods use Server-Sent Events.

## Methods (§7)

| Method | Purpose |
|---|---|
| `a2a.SendMessage` | Send a message; create a task |
| `a2a.SendStreamingMessage` | Send + receive task stream via SSE |
| `a2a.GetTask` | Poll task state |
| `a2a.ListTasks` | List tasks |
| `a2a.CancelTask` | Cancel a running task |
| `a2a.SubscribeToTask` | SSE stream for an existing task |
| `a2a.GetExtendedAgentCard` | Authenticated card with extra fields |

## Task states (§6.4)

`TASK_STATE_SUBMITTED` → `TASK_STATE_WORKING` → `TASK_STATE_COMPLETED`
Other terminal: `FAILED`, `CANCELED`, `REJECTED`, `INPUT_REQUIRED`, `AUTH_REQUIRED`.

## Message / Part / Artifact (§6.1–6.6)

A `Message` carries `parts` (text / file / data) and a `role` (`ROLE_USER` or `ROLE_AGENT`). The peer replies with one or more `Artifact`s when the task reaches `COMPLETED`.

## Error codes (§8)

`-32001` TaskNotFound · `-32002` TaskNotCancelable · `-32003` PushNotificationsNotSupported · `-32004` UnsupportedOperation · `-32005` ContentTypeNotSupported · `-32006` InvalidAgentResponse · `-32099..-32000` JSON-RPC reserved.

## What a2abridge implements today

- Agent Card on `/.well-known/a2a` (§5)
- All eight JSON-RPC methods (§7)
- TaskState enum, Message/Part/Artifact, StreamResponse one-of
- Header `A2A-Version: 1.0`
- Loopback by default, no auth (cross-machine + mTLS in roadmap Phase 2)

Not yet implemented: Push Notifications (§9.5), gRPC binding (§7.2), HTTP+REST binding (§7.3), Extended Agent Card with post-auth fields.
