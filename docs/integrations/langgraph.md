# LangGraph ⇄ a2abridge

LangGraph supports A2A as one of its native transports starting with
0.4.x. A LangGraph agent (the compiled `Graph` object) can be exposed
as an A2A peer with `langgraph.adapters.a2a.A2AAdapter`.

## Expose a LangGraph graph as an A2A peer

```python
from langgraph.adapters.a2a import A2AAdapter
import requests

adapter = A2AAdapter(
    graph=my_compiled_graph,
    name="langgraph-research",
    skills=["search", "summarize"],
    bind="127.0.0.1:0",
)
adapter.start()

requests.post(
    "http://127.0.0.1:7777/register",
    json={"url": adapter.url},
    timeout=2,
)
```

The graph is now reachable from Claude Code / Codex / Cursor through
their `a2a_send_message` MCP tool — the bridges will see it in
`a2a_list_agents` automatically.

## Call other peers from a LangGraph node

```python
from langgraph.adapters.a2a import A2AClient

def review_node(state):
    client = A2AClient.from_directory(
        "http://127.0.0.1:7777", skills_filter=["review"]
    )
    task = client.send_message(text=state["plan"], blocking=True)
    return {"review": task.artifacts[0].parts[0].text}
```

`A2AClient.from_directory` queries `/agents`, then resolves each peer's
Agent Card and picks the first one whose `skills` overlap the filter.

## Streaming

LangGraph's stream nodes map cleanly onto `a2a.SendStreamingMessage`.
The adapter exposes `adapter.stream_send(...)` which yields each
`statusUpdate` / `artifactUpdate` event.

## Caveats

- The adapter requires `langgraph >= 0.4.7` — earlier versions used a
  pre-spec wire format.
- LangGraph's Agent Card omits `Provider.organization` by default; set
  it explicitly so users know which agent they're seeing in `a2abridge doctor`.
