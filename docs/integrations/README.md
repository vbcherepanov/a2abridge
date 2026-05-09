# a2abridge — third-party integrations

These docs show how to plug native-A2A frameworks into your local
a2abridge mesh. Every integration shares the same pattern:

1. Start the framework's A2A adapter on a local port.
2. POST that URL to `http://127.0.0.1:7777/register`.
3. Profit — Claude Code / Codex / Cursor / Cline / Continue / Gemini can
   now discover and call the framework via `a2a_list_agents` /
   `a2a_send_message`.

| Framework | Native A2A | Doc |
|---|---|---|
| Google ADK 1.0 | yes (1.0+) | [google-adk.md](google-adk.md) |
| LangGraph | yes (0.4.7+) | [langgraph.md](langgraph.md) |
| CrewAI | yes (0.95+) | [crewai.md](crewai.md) |
| LlamaIndex Agents | yes (0.13+) | [llamaindex.md](llamaindex.md) |
| Semantic Kernel | yes (1.20+) | (coming) |
| AutoGen | yes (0.4+) | (coming) |
| BeeAI Framework | yes (post-ACP merge) | (coming) |

## Cross-machine setup

Set `A2A_MDNS=1` on every peer (and bridge) on the LAN. Each speaks
`_a2a._tcp.local.` so peers find each other without a shared directory.
For WAN, point each adapter's `A2A_DIRECTORY` at a single internet-reachable
`a2abridge directory` instance and configure mTLS via `A2A_TLS_*` —
see the main [README → Security model](../../README.md#security-model).

## Consistency caveat

The framework versions listed above are the minimum for stable A2A
interop as of May 2026. Always verify the framework's Agent Card on
`/.well-known/a2a` returns `"protocolVersion": "1.0"` and
`"capabilities.streaming": true` before pointing production traffic at
it.
