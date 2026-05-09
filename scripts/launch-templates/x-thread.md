# X / Twitter launch thread

Each tweet is ≤280 chars. Numbers in [brackets] are the tweet count.

---

**[1/8]** I open-sourced a2abridge — the missing bridge between your AI
coding agents.

Claude Code, Codex CLI, Cursor, Cline, Continue, Gemini — all on one
A2A 1.0 mesh. They discover each other, dispatch tasks, share inboxes.
No copy-paste between windows.

🔗 github.com/vbcherepanov/a2abridge

---

**[2/8]** The protocol war ended last August: IBM's ACP merged into
Google's A2A under Linux Foundation. ADK, LangGraph, CrewAI, LlamaIndex
all speak it natively now.

Closed bridges trap you on one stack. Open bridges let you mix vendors
on a real spec.

---

**[3/8]** What's in v2.0:

→ single Go binary, 11 subcommands
→ kardianos/service supervisor (mac/linux/win/wsl2)
→ 6 IDE writers with .bak backups
→ mTLS + ed25519 federation
→ mDNS LAN auto-discovery
→ Push Notifications + SSE fast-path
→ PII scrub on every outbound
→ embedded Claude Code skill

---

**[4/8]** The "killer feature": UserPromptSubmit hook auto-injects the
A2A inbox before every Claude Code prompt.

You don't have to ask "any messages from peers?" — they just appear at
the top of your next turn. Frontend Claude learns about a backend API
change before it touches the UI.

---

**[5/8]** Cross-machine? Run `a2abridge service install --federation`,
share certs, set `A2A_TRUST_ROOTS`, done. mTLS over TLS 1.3 with
RequireAndVerifyClientCert + per-peer CN/SAN allow-list.

For LAN: `A2A_MDNS=1` and they find each other via DNS-SD.

---

**[6/8]** Always-online agent? `a2abridge worker start --cmd claude`
runs Claude in detached tmux. Survives every IDE restart, picks up
tasks from peers 24/7. No API key needed — uses your subscription.

---

**[7/8]** Install:

  curl -fsSL https://raw.githubusercontent.com/vbcherepanov/a2abridge/main/install.sh | bash

Or `iwr -useb https://.../install.ps1 | iex` on Windows.
44 test cases pass under -race. MIT, no telemetry.

---

**[8/8]** Massive thanks to:

@LF_AIDataFndn for shepherding A2A 1.0 to a stable spec
@kardianos for the cross-platform service lib
@mark3labs for mcp-go

If you've been juggling agents — try it and DM me what breaks. Stars
help if you find it useful.

🔗 github.com/vbcherepanov/a2abridge

---

**Tags to add to first tweet**: #ClaudeCode #LLMOps #MultiAgent #OpenSource #Golang
