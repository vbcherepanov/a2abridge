# A2A 1.0 — protocol cheatsheet

a2abridge speaks the [Agent2Agent 1.0 specification](https://a2a-protocol.org/latest/specification/) under the Linux Foundation, through the official [a2a-go](https://github.com/a2aproject/a2a-go) SDK. This file is a quick reference; the spec is authoritative.

## Transport

Two bindings on the same base URL, both listed in the Agent Card's `supportedInterfaces`:

- **JSON-RPC 2.0** at `POST /` with `Content-Type: application/json`.
- **HTTP+JSON** at resource paths (`POST /message:send`, `GET /tasks/{id}`, ...).

Clients send the header `A2A-Version: 1.0`. Streaming methods answer with Server-Sent Events.

## Methods

| JSON-RPC method | HTTP+JSON | Purpose |
|---|---|---|
| `SendMessage` | `POST /message:send` | Send a message; creates a task |
| `SendStreamingMessage` | `POST /message:stream` | Send + receive the task's events via SSE |
| `GetTask` | `GET /tasks/{id}` | Poll task state |
| `ListTasks` | `GET /tasks` | List tasks (filter by context or state, paged) |
| `CancelTask` | `POST /tasks/{id}:cancel` | Cancel a running task |
| `SubscribeToTask` | `POST /tasks/{id}:subscribe` | SSE stream for a task that is still running |
| `CreateTaskPushNotificationConfig` / `GetTaskPushNotificationConfig` / `ListTaskPushNotificationConfigs` / `DeleteTaskPushNotificationConfig` | `/tasks/{id}/pushNotificationConfigs[/{configId}]` | Manage push-notification webhooks |
| `GetExtendedAgentCard` | `GET /extendedAgentCard` | Authenticated card (not offered by a2abridge) |

## Conversation semantics

- Every `SendMessage` without `taskId` creates a new task. On a bridge that task stays `TASK_STATE_WORKING` until the receiving agent answers with `a2a_complete_task`.
- A task runs one execution at a time: a message to a task that is still being worked on is rejected, and so is a message to a finished task.
- Continue a conversation by sending a new message with the same `contextId`. Set `taskId` only for a task in `TASK_STATE_INPUT_REQUIRED`.
- `configuration.returnImmediately: true` returns as soon as the task exists; otherwise the call waits for a terminal or interrupted state.

## Task states

`TASK_STATE_SUBMITTED` → `TASK_STATE_WORKING` → `TASK_STATE_COMPLETED`
Other terminal: `TASK_STATE_FAILED`, `TASK_STATE_CANCELED`, `TASK_STATE_REJECTED`. Interrupted: `TASK_STATE_INPUT_REQUIRED`, `TASK_STATE_AUTH_REQUIRED`.

## Message / Part / Artifact

A `Message` has `messageId`, `role` (`ROLE_USER` or `ROLE_AGENT`), `parts`, and optional `taskId`, `contextId` and `metadata`. A Part holds exactly one content field:

```json
{"text": "..."}
{"raw": "<base64>", "filename": "output.txt", "mediaType": "text/plain"}
{"url": "https://...", "mediaType": "text/plain"}
{"data": {"...": "..."}}
```

A bridge answers with one text `Artifact` named `reply` and the same text as the completed status message. Stream events are one-of `task`, `message`, `statusUpdate`, `artifactUpdate`.

## Error codes

`-32001` TaskNotFound · `-32002` TaskNotCancelable · `-32003` PushNotificationNotSupported · `-32004` UnsupportedOperation · `-32005` ContentTypeNotSupported · `-32006` InvalidAgentResponse · `-32007` ExtendedAgentCardNotConfigured · `-32008` ExtensionSupportRequired · `-32009` VersionNotSupported · `-32700` / `-32600` / `-32601` / `-32602` / `-32603` JSON-RPC standard errors.

## What a2abridge implements today

- Agent Card on `/.well-known/agent-card.json` with JSON-RPC and HTTP+JSON interfaces and the streaming and push-notification capabilities
- Every method above except the extended card
- Push notifications: each task event is POSTed as a `StreamResponse` with the `A2A-Notification-Token` header and retried on network errors and 5xx
- Terminal tasks stay readable for 30 minutes after their last update
- A request whose `A2A-Version` is not 1.0 gets `VersionNotSupportedError` (`-32009`); a request without it counts as 0.3 and is rejected too
- The Agent Card carries `Cache-Control`, `ETag` and `Last-Modified`
- Loopback by default; opt-in cross-machine federation via mTLS + ed25519 (`A2A_TLS_CERT` / `A2A_TLS_KEY` / `A2A_TRUST_ROOTS`)

Not yet implemented: gRPC binding.
