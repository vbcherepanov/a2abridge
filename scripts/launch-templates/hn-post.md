# Hacker News submission

**Title** (≤80 chars):

> Show HN: a2abridge — open A2A protocol bridge for AI coding agents

**URL**: https://github.com/vbcherepanov/a2abridge

**Comment** (post immediately as the first reply):

> Author here. I had Claude Code in one window, Codex CLI in another, and
> a Cursor session in a third — all working on the same codebase, none of
> them aware of each other. Whenever one finished a refactor I'd
> copy-paste the diff into the next agent's prompt manually. That got old.
>
> a2abridge wraps each running agent into a peer that speaks the open
> Linux Foundation A2A 1.0 protocol — the same standard ADK 1.0,
> LangGraph, CrewAI and LlamaIndex use natively. Peers find each other
> through a local directory, dispatch tasks back and forth via JSON-RPC,
> share an inbox, and (optionally) talk across machines over mTLS.
>
> Single Go binary, kardianos/service supervisor for mac/linux/windows,
> 6 IDE writers (Claude Code, Codex CLI, Cursor, Cline, Continue, Gemini),
> embedded skill + UserPromptSubmit hook so the inbox shows up
> automatically before each prompt. PII screen, push notifications,
> SSE fast-path, mDNS LAN discovery, ed25519 cert generation. 44 test
> cases under -race.
>
> Install: `curl -fsSL https://raw.githubusercontent.com/vbcherepanov/a2abridge/main/install.sh | bash`
>
> Why open: closed bridges (Anthropic Agent Teams, CCB, ruflo) lock you
> to one stack. The protocol war ended in Aug 2025 when IBM's ACP merged
> into Google's A2A under Linux Foundation — there's an open standard now;
> bridges should speak it.
>
> Curious what gaps you'd hit. Especially interested in feedback from
> teams running >2 agents in parallel.

**Tag**: `Show HN`

**Best post window**: Tuesday-Thursday 09:00-12:00 PST (US morning).
Avoid Friday afternoon and weekends — HN traffic peaks during US working
hours. Don't farm upvotes; first 30 minutes determine front-page or not.
