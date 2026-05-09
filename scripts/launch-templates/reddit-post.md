# Reddit submission

Best subs (in order of relevance):

1. **r/ClaudeAI** — biggest concentration of Claude Code users
2. **r/LocalLLaMA** — large multi-agent / open-source LLM crowd
3. **r/golang** — for the Go-implementation angle
4. **r/MachineLearning** (Self-promotion Saturday only)
5. **r/programming** — broad reach but heavy moderation

---

**Title (r/ClaudeAI / r/LocalLLaMA):**

> I built an open A2A protocol bridge so Claude Code, Codex, Cursor and Gemini can finally talk to each other

**Title (r/golang):**

> a2abridge — single Go binary that turns every AI coding agent on your machine into an A2A 1.0 peer

**Body (same for all):**

> The pain: I had three different AI coding agents (Claude Code in IDE,
> Codex CLI, Cursor) working on the same codebase, none of them aware of
> what the others were doing. Copy-pasting diffs and explanations between
> windows wasn't scaling.
>
> The fix: wrap each agent into a peer of the open
> [A2A 1.0 protocol](https://a2a-protocol.org/latest/) (the Linux Foundation
> standard that ADK / LangGraph / CrewAI / LlamaIndex already use). They
> register with a local directory, find each other automatically, dispatch
> tasks via JSON-RPC, share an inbox.
>
> What's in v2.0:
> - Single Go binary, 11 subcommands, runs as a system service on macOS / Linux / Windows / WSL2
> - 6 IDE writers (Claude Code, Codex CLI, Cursor, Cline, Continue, Gemini CLI)
> - mTLS + ed25519 federation for cross-machine setups
> - mDNS / DNS-SD LAN auto-discovery
> - Push Notifications (A2A 1.0 §9.5) with retry + backoff
> - PII / secret screen (11 regex detectors) before any outbound message
> - Embedded Claude Code skill + UserPromptSubmit hook (inbox auto-injected before each prompt)
> - 44 test cases under `-race`, GitHub Actions matrix builds
>
> Repo: https://github.com/vbcherepanov/a2abridge
> Install: `curl -fsSL https://raw.githubusercontent.com/vbcherepanov/a2abridge/main/install.sh | bash`
>
> Closed competitors (Anthropic Agent Teams, CCB, ruflo) work great
> within their stack. a2abridge is the bridge that lets you mix vendors
> on the same protocol — open code, MIT, no telemetry.
>
> Genuinely curious about edge cases I haven't hit yet.

**Best post day**: Tuesday-Thursday morning local subreddit time.

**Reddit etiquette**: read the rules tab of each sub before posting.
r/ClaudeAI requires an `[OC]` flair for original projects, r/LocalLLaMA
prefers self-posts over link-only, r/golang doesn't allow self-promotion
without engaging in comments.
