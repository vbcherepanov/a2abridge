# LlamaIndex Agents ⇄ a2abridge

LlamaIndex Agents 0.13+ ship `llama_index.agent.transports.A2A` that
turns any `AgentRunner` into an A2A peer. Indexes / retrievers stay
local; only the Agent's `chat()` surface is exposed.

## Expose a LlamaIndex agent

```python
from llama_index.agent import OpenAIAgent
from llama_index.agent.transports.a2a import A2AAgentServer
import requests

agent = OpenAIAgent.from_tools(...)
server = A2AAgentServer(
    agent,
    name="llama-rag",
    skills=["retrieve", "answer"],
    bind="127.0.0.1:0",
)
server.start()

requests.post(
    "http://127.0.0.1:7777/register",
    json={"url": server.url},
    timeout=2,
)
```

The agent now answers `a2a.SendMessage` calls. Each call is one
`agent.chat(text)` invocation; the response is returned as a final
`artifact` part with `MediaType: text/plain`.

## Use a Claude Code bridge as a LlamaIndex tool

```python
from llama_index.agent.transports.a2a import A2APeerTool

tool = A2APeerTool(
    peer_url="http://127.0.0.1:55477",  # a Claude Code bridge
    name="claude_code",
    description="Delegate refactoring tasks to a Claude Code session.",
)

agent = OpenAIAgent.from_tools([tool, *other_tools])
```

## Streaming

LlamaIndex's `astream_chat` becomes `a2a.SendStreamingMessage`. The
A2APeerTool surfaces each chunk as a `StreamingResponse` from the tool
call.

## Caveats

- The LlamaIndex Agent Card defaults `streaming=True` even for non-async
  agents; if you don't actually support streaming, override it via
  `A2AAgentServer(..., card_overrides={"capabilities": {"streaming": false}})`.
- Index reloads (`agent.reset()`) do not propagate to peers automatically.
  Send a sentinel message or tear down + re-register the server if you
  want peers to re-fetch the Agent Card.
