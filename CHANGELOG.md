# Changelog

All notable changes to a2abridge are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [4.0.1] — 2026-09-15

### Fixed

- **`go install` builds report their module version.** A binary installed
  with `go install github.com/vbcherepanov/a2abridge/v4/cmd/a2abridge@v4.0.0`
  carries no release ldflags and printed the stale default `3.0.0-dev`. That
  value also went into the Agent Card and MCP server version, `doctor`,
  `service status` and the help header, and `a2abridge update` compared
  against it. The version now comes from release ldflags, then from the module
  version the Go toolchain records (pseudo-versions as-is), then `dev`. Commit
  and build date fall back to the VCS stamp (`-dirty` for a modified tree),
  then `unknown`.
- `a2abridge update` ignores build metadata such as `+dirty` when comparing
  versions, so toolchain-stamped builds still compare against releases.

## [4.0.0] — 2026-09-15

### Changed (breaking)

- **Module path is now `github.com/vbcherepanov/a2abridge/v4`**, as Go
  requires for major version 2 and above. Install with
  `go install github.com/vbcherepanov/a2abridge/v4/cmd/a2abridge@latest`;
  the old path resolves only to v1.1.0 on the Go module proxy.
- **A2A 1.0 wire format through the official Go SDK.** The hand-written
  protocol layer is replaced by [a2a-go](https://github.com/a2aproject/a2a-go)
  v2.5.0. Bridges serve the JSON-RPC binding at `POST /` with the 1.0 method
  names (`SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`,
  `CancelTask`, `SubscribeToTask`, `CreateTaskPushNotificationConfig`,
  `GetTaskPushNotificationConfig`, `ListTaskPushNotificationConfigs`,
  `DeleteTaskPushNotificationConfig`, `GetExtendedAgentCard`) and the
  HTTP+JSON binding (`POST /message:send`, `GET /tasks/{id}`, ...). The Agent
  Card lists both in `supportedInterfaces`. Enum values, the Part union and
  error codes follow the 1.0 schema, so 3.x and 4.0 bridges cannot talk to
  each other: upgrade every bridge on the mesh together.
  `a2abridge update` and `install.sh` pick the latest release and cross this
  major version; pin `--version v3.1.0` (or `A2A_VERSION=v3.1.0`) to stay on 3.x.
- **Removed** the non-spec REST API under `/v1/`, the legacy method names
  (`message/send`, `tasks/get`, `a2a.SendMessage`, ...) and the legacy card
  path `/.well-known/a2a`.
- **One message per task.** A task stays `working` until the host answers
  with `a2a_complete_task`, and a message to a task that is still being worked
  on (or already finished) is rejected. Continue a conversation by sending a
  new message with the same `context_id`; `task_id` is only for a task
  awaiting input. A redelivered `messageId` is still queued once; the
  redelivery's own task ends `rejected`.
- **MCP inbox entries.** `a2a_inbox` returns flat objects
  `{messageId, taskId, contextId, from, text, kind?, state?, ts?}` instead of
  A2A `Message` objects. The on-disk `inbox.json` snapshot format is unchanged.
- **Push notifications** POST an A2A 1.0 `StreamResponse` with the
  `A2A-Notification-Token` header (was `X-A2A-Token`). With federation TLS
  enabled, webhook URLs resolving to loopback, private or link-local
  addresses are refused unless `A2A_PUSH_ALLOW_PRIVATE=1`.
- **`A2A-Version` is required.** A request whose `A2A-Version` header (or
  request parameter) is not 1.0 in Major.Minor gets
  `VersionNotSupportedError` (`-32009` / HTTP 400). A request without it is
  read as 0.3, as the spec prescribes, and rejected the same way. The a2a-go
  and a2a-sdk clients send the header on every request.
- `SubscribeToTask` on an unknown task answers `TaskNotFoundError` and on a
  finished task `UnsupportedOperationError`, as a plain error instead of an
  SSE stream carrying the error.
- Task status timestamps are serialized in UTC (`...Z`).
- **Request limits.** A2A request bodies are capped at 10 MiB (`413` /
  JSON-RPC `-32600` beyond that) and reading a request is bounded by a
  one-minute timeout; SSE responses are not affected.

### Added

- Antigravity CLI (`agy`) support: `a2abridge install` detects it by
  `~/.gemini/config/mcp_config.json` or `~/.gemini/antigravity-cli` and
  writes the MCP block to `mcp_config.json`; `uninstall` and `doctor` cover
  it too (`--ide antigravity`) (#19).
- `cmd/a2abridge-tck` (build tag `tck`): the bridge's HTTP stack with an
  executor for the official A2A TCK scenarios
  (`go build -tags tck ./cmd/a2abridge-tck`).
- The Agent Card is served with `Cache-Control: public, max-age=300`, a
  strong `ETag` and `Last-Modified`; `If-None-Match` answers `304`.

## [3.1.0] — 2026-09-15

Reliability release built on community fixes by
[@terafin](https://github.com/terafin) (#9, #10, #12, #13, #14, #15, #18, #20).

### Fixed

- **Durable inbox.** The snapshot is restored on start and no longer deleted
  on shutdown, redelivered messages are de-duplicated by `messageId`, and the
  inbox is soft-capped at 500 entries (#12, #13).
- **Outgoing-reply notifications no longer linger.** `CompleteTask` clears
  them, contentless completions are skipped, delivered notifications age out,
  and a peek consumes them once (#9, #10, #18).
- **A restored reply notification keeps its delivery timestamp**, so a bridge
  restart does not resurrect it on every wake.
- **A bridge no longer outlives its MCP host.** It shuts down on stdin EOF and,
  on Linux, on the parent-death signal (#20).
- **One bridge per agent.** A second bridge on a held address retries briefly,
  then defers to the incumbent and exits 0 (#20). On Windows the held port is
  detected through `WSAEADDRINUSE`.
- `a2abridge doctor` no longer suggests `chmod +x` for the hook on Windows (#19).

### Added

- Per-bridge `/metrics` endpoint (#14).
- `A2A_NUDGE=dtach` backend with `A2A_NUDGE_SOCKET` (#15).

### Changed

- The inbox snapshot is a single `inbox.json` per state directory instead of
  `inbox-<ppid>.json`. The hook still reads legacy files and prunes those whose
  process is gone.
- golangci-lint configuration migrated to the v2 format.

### Tests

- Windows: POSIX permission assertions are skipped and home-directory tests
  set `USERPROFILE`, restoring a green Windows build.
- Integration tests for parent death and single-bridge behaviour
  (`go test -tags=integration ./internal/cli/`).

## [3.0.0] — 2026-06-10

Spec-compliance and hardening release. The JSON-RPC wire format now
matches the A2A 1.0 specification exactly, which is a breaking change
for v2.x peers (legacy method names are still accepted as aliases, but
enum values and the Part union changed on the wire). Upgrade all peers
in a mesh together.

### Changed — A2A 1.0 spec compliance (breaking)

- **JSON-RPC method names** follow the spec: `message/send`,
  `message/stream`, `tasks/get`, `tasks/cancel`, `tasks/resubscribe`,
  `tasks/pushNotificationConfig/{set,get,list,delete}`,
  `agent/getAuthenticatedExtendedCard`. Old `a2a.*` names are accepted
  as deprecated aliases for rolling upgrades. `tasks/list` remains a
  documented non-spec extension.
- **TaskState / Role wire values** are now the spec JSON forms
  (`submitted`, `working`, `input-required`, `completed`, `canceled`,
  `failed`, `rejected`, `auth-required`; `user` / `agent`) instead of
  proto-style enum strings.
- **Agent card** is served at `/.well-known/agent-card.json` (spec
  location); `/.well-known/a2a` is kept as a legacy alias and the
  client falls back to it on 404.
- **Part** serializes as the spec discriminated union
  (`{"kind":"text"|"file"|"data", ...}` with nested
  `file:{bytes,uri,mimeType,name}`); the legacy flat shape is still
  accepted on input. `Message`/`Task` now carry `kind` discriminators.
- Spec error codes are reachable: `-32002 TaskNotCancelable`,
  `-32003 PushNotSupported`, `-32004 UnsupportedOperation`,
  `-32602 InvalidParams` via exported sentinel errors.
- REST endpoints under `/v1/` are now documented as a non-spec
  convenience API (the spec binding is JSON-RPC at `POST /`).

### Security

- **PII screening**: private-key redaction now removes the whole PEM
  block (previously only the BEGIN marker was stripped while the key
  body leaked through); screening is applied to **all** outbound paths
  (`a2a_send_message`, `a2a_send_streaming`, `a2a_complete_task` —
  previously only the first).
- **mTLS**: enabling TLS without `A2A_TRUST_ROOTS` is now a hard
  configuration error instead of silently verifying client certs
  against the system root pool. Allow-list matching also considers IP
  SANs. Certificate serials come from crypto/rand.
- **Self-update and installers verify SHA256**: releases now publish
  `checksums.txt`; `a2abridge update`, `install.sh` and `install.ps1`
  refuse unverified archives. `A2A_REPO` override prints a loud warning.
- Inbox snapshots are written `0600`; IDE config writes preserve
  original permissions (new files `0600`) and are atomic
  (temp + rename). `.a2a/` state dirs get a `.gitignore`.

### Fixed

- Double delivery race between the SSE fast path and outgoing-task
  polling; the `on-outgoing-reply` hook now also fires on the polling
  path.
- The headless responder no longer answers its own synthetic
  outgoing-reply messages (each one previously spawned a paid LLM run).
- Terminal tasks and push configs are evicted after a TTL (30 min)
  instead of growing without bound; messages to terminal tasks are
  rejected with `-32002`.
- Streaming errors are no longer swallowed: server-side handler
  failures surface as JSON-RPC errors (pre-stream) or a final SSE error
  event (mid-stream); the client detects non-SSE error responses and
  joins multi-line `data:` frames per the SSE spec.
- `Client.call` honors the configured `http.Client` (custom TLS/proxy
  transports were silently ignored); directory calls have 5 s timeouts;
  peer agent cards are fetched concurrently; graceful unregister works
  during shutdown.
- All POST bodies are capped (8 MiB) with `http.MaxBytesReader`;
  client reads are bounded.
- Continue IDE was never auto-detected (and never uninstalled) because
  directory detection was broken; Claude Code MCP registration goes to
  `~/.claude.json` (the file Claude Code actually reads) while hooks
  stay in `~/.claude/settings.json`.
- JSON config merging preserves integers larger than 2^53
  (`json.Number` round-trip) instead of corrupting them to floats.
- Binary self-replacement is atomic with checksum verification, bounded
  extraction and real semver comparison; `update --check` exits 1 when
  an update is available.
- `service stop` for the directory now actually waits for graceful
  shutdown (works under Windows SCM, which has no signal path).
- tmux worker prompts are sent literally (`send-keys -l --`) so
  key-name-like prompts aren't interpreted, with a startup delay so the
  TUI doesn't swallow input.
- Prometheus metrics are actually incremented (7 of 9 counters were
  never wired); the peers gauge updates on register/unregister/GC.
- Docker images embed the release version via ldflags (previously
  reported `0.2.0-dev`); `windows/arm64` added to the release matrix.
- Many smaller fixes: rune-safe truncation, AppleScript newline
  escaping, mDNS TXT URL validation, directory registry URL validation
  and stoppable GC, consistent help/exit codes, dead code removal.

## [2.0.0] — 2026-05-09

Ecosystem release. Adds the worker daemon and the third-party
integration playbook so teams can wire ADK / LangGraph / CrewAI /
LlamaIndex peers into their local mesh.

### Added

- **`a2abridge worker {start|stop|status|attach}`** — runs an
  always-online Claude (or any CLI) inside a detached tmux session.
  Survives IDE restarts, exposes its MCP tools to the directory like
  any other peer, and can be seeded with an initial prompt via
  `--prompt "..."`.
- **Integration docs** under `docs/integrations/`:
  - `google-adk.md` — ADK 1.0 ⇄ a2abridge
  - `langgraph.md` — LangGraph 0.4.7+ ⇄ a2abridge
  - `crewai.md` — CrewAI 0.95+ ⇄ a2abridge
  - `llamaindex.md` — LlamaIndex Agents 0.13+ ⇄ a2abridge
  - `README.md` — index + cross-machine setup notes

### Removed

- Nothing. v2.0 is purely additive on top of v1.1.

## [1.1.0] — 2026-05-09

Cross-machine + reliability follow-up. Same single binary, additive only.

### Added

- **Push notifications now retry** with exponential backoff (200 ms →
  3.2 s, 5 attempts, per-attempt 5 s timeout) on 5xx and network errors.
  4xx responses are treated as permanent and not retried.
- **`a2abridge service install --federation`** generates an ed25519
  cert + key under `~/.a2abridge/tls` and prints the env block to wire
  TLS into your IDE config in one step. Optional `--cn <name>` overrides
  the hostname-derived common name.
- **mDNS / DNS-SD discovery** (`A2A_MDNS=1`): bridges publish themselves
  on `_a2a._tcp.local.` and discover LAN peers without a shared directory.
  Useful on multi-laptop dev setups and conference Wi-Fi.

### Changed

- Bumped `golang.org/x/net` (transitive via mDNS) to v0.54+ so the
  `syscall.recvmsg` linker error on Go 1.25 is gone.

## [1.0.0] — 2026-05-09

First production-ready release. The previous 0.1.0-line single-machine
prototype has been rewritten end-to-end into a multi-OS, multi-IDE,
spec-complete A2A 1.0 bridge.

### Added — single binary

- `a2abridge` is now a single multi-command binary. Subcommands:
  `bridge`, `directory`, `install`, `uninstall`, `update`, `service`,
  `doctor`, `cert`, `completion`, `version`.
- `a2abridge service install/start/stop/status/uninstall` runs the
  directory daemon under launchd (macOS), systemd-user (Linux/WSL2) and
  Windows Service Manager — backed by [`kardianos/service`](https://github.com/kardianos/service).
- `a2abridge install [--apply]` auto-detects 6 IDEs and writes the MCP
  block into each, with timestamped `.bak` backups:
  Claude Code, Codex CLI, Cursor, Cline (VS Code), Continue, Gemini CLI.
- `a2abridge install` also drops the `a2a-bridge` skill and the
  UserPromptSubmit hook, registering the hook in `~/.claude/settings.json`.
- `a2abridge uninstall [--purge]` reverses the install — strips the MCP
  block from every config, removes skill/hook, stops + uninstalls the
  service. Without `--purge` everything is renamed to `.bak.<ts>`.
- `a2abridge update [--check]` self-updates from the latest GitHub
  release, atomically replacing the binary with rollback on failure.
- `a2abridge doctor` runs a 9-check health audit — directory liveness,
  IDE configs, skill, hook, version.
- `a2abridge completion bash|zsh|fish|powershell` emits a tab-completion
  script.
- `install.sh` / `install.ps1` one-line installers for macOS / Linux /
  WSL2 / Windows.

### Added — protocol

- Full A2A 1.0 RPC + REST coverage:
  - JSON-RPC: `SendMessage`, `SendStreamingMessage`, `GetTask`,
    `ListTasks`, `CancelTask`, `SubscribeToTask`,
    `GetExtendedAgentCard`, plus the four
    `*TaskPushNotificationConfig` methods (§9.5).
  - HTTP+REST mirror at `/v1/tasks`, `/v1/tasks/{id}`,
    `/v1/tasks/stream`, `/v1/agent`, `/v1/tasks/{id}/push` (§7.3).
- **Push Notifications** (§9.5): peers register webhooks; bridges POST
  status updates as they happen. Supports `X-A2A-Token` shared secret
  and pluggable `Authorization` schemes.
- **SSE fast-path for outbound replies**: `a2a_send_message` opens a
  `SubscribeToTask` stream on the peer, so a peer's `COMPLETED` lands
  in the local inbox in milliseconds instead of waiting for the
  5-second polling tick (which is still in place as a safety net).

### Added — federation (Phase 2)

- **mTLS + ed25519** via `A2A_TLS_CERT`, `A2A_TLS_KEY`, `A2A_TRUST_ROOTS`,
  `A2A_PEER_ALLOW`. When set, the bridge serves over TLS 1.3 with
  required client certs and validates the peer CN/SAN against the
  allow-list.
- `a2abridge cert generate [--cn <name>] [--dir <path>]` produces an
  ed25519 self-signed cert + key (10-year validity) ready to wire into
  `A2A_TLS_CERT` / `A2A_TLS_KEY`.
- `a2a.DefaultTransport` package-level hook so every outbound RPC client
  inherits the federation TLS config without per-call plumbing.

### Added — security

- **PII / secret screen** (`internal/security/pii.go`): outbound
  messages pass through 11 regex detectors (AWS / GitHub / Anthropic /
  OpenAI / Google / Stripe / Slack / JWT / PEM private key / etc.).
  Matches are replaced with `[REDACTED:<name>]` before send and surfaced
  in MCP metadata so the model can warn the user.
- **User hook scripts**: drop `~/.a2abridge/hooks/{on-inbound,on-outgoing-reply}.sh`
  (or `.ps1`/`.cmd` on Windows) and the bridge fires them on every
  inbound message and outbound reply, with the JSON payload on stdin and
  in `A2A_EVENT_*` env vars. Bounded by a 5-second timeout.

### Added — state

- **Project-local state**: inbox, label and bridge log live in `./.a2a/`
  inside CWD, falling back to `~/.a2abridge/state/<ppid>/` when CWD is
  not writable. Old `/tmp/a2a-*` files are gone.

### Added — tests + CI

- 35 unit + integration test cases across 6 packages, all passing under
  `-race`.
- GitHub Actions: `build.yml` runs the test matrix on
  `ubuntu/macos/windows × 1.25`; `release.yml` cross-compiles the
  release artefacts (`darwin/{amd64,arm64}`, `linux/{amd64,arm64}`,
  `windows/amd64`) on `v*` tag push and attaches them to the release.

### Removed

- `cmd/directory` and `cmd/bridge` separate binaries — replaced by
  subcommands of the unified `a2abridge` binary. Existing `~/.claude/settings.json`
  entries pointing at the old paths must be re-applied via
  `a2abridge install --apply`.
- `/tmp/a2a-bridge.log` and `/tmp/a2a-inbox-*.json` paths — see
  project-local `.a2a/` above.

### Compatibility

| Platform | Verified |
|---|---|
| macOS 13+ (Intel & Apple Silicon) | yes |
| Ubuntu 22.04+ / Debian 12+ / Fedora 40+ | yes |
| Windows 11 | yes |
| WSL2 (Ubuntu) with `systemd=true` | yes |

---

## [0.1.0] — 2026-04-14

Initial single-machine prototype. Two binaries (`a2a-directory` +
`a2a-bridge`), launchd-only, no installer, no tests, no skill, no
federation. Superseded by 1.0.0.
