# Changelog

All notable changes to a2abridge are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
