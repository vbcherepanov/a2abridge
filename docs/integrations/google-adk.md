# Google ADK 1.0 ⇄ a2abridge

The Agent Development Kit ([google/adk](https://github.com/google/adk-python),
GA April 2026) speaks A2A natively, so an ADK agent can join your local
a2abridge mesh as a first-class peer with no glue code. The bridge
discovery service is just an extra A2A directory; ADK agents either
register on it or you point their `peer_url` at your bridge URL directly.

## Register an ADK peer with the local directory

```python
import requests
from adk import Agent
from adk.transports.a2a import A2AServer

agent = Agent(
    name="adk-data-agent",
    skills=["sql", "etl"],
    model="gemini-2.5-pro",
)

server = A2AServer(agent, host="127.0.0.1", port=0)  # random port
server.start()                                        # background thread

# Register with a2abridge's local directory so other peers see us.
requests.post(
    "http://127.0.0.1:7777/register",
    json={"url": server.url},
    timeout=2,
)
```

The ADK agent's Agent Card on `/.well-known/a2a` is now discoverable to
every Claude/Codex bridge on the same machine. `a2abridge doctor` will
list it as one of the registered peers.

## Talk to a Claude bridge from ADK

ADK ships an `A2AClient` that's a drop-in JSON-RPC peer client:

```python
from adk.transports.a2a import A2AClient

# Get peer URLs from the directory
peers = requests.get("http://127.0.0.1:7777/agents").json()
claude_peer = next(p for p in peers if "claude" in p["url"])

client = A2AClient(claude_peer["url"])
task = client.send_message(
    text="Refactor the SQL in src/queries/orders.go for performance.",
    blocking=True,
)
print(task.artifacts[0].parts[0].text)
```

## Handling inbound Claude requests inside ADK

Override `Agent.handle_message` (or use ADK's `@on_message` decorator) —
no a2abridge-specific code is needed; ADK already adheres to the A2A
spec.

## mDNS cross-machine

Set `A2A_MDNS=1` on both sides so they discover each other on the LAN
without a shared directory. ADK's A2AServer publishes `_a2a._tcp.local.`
out of the box; a2abridge consumes those records when started with the
same env flag.

## Caveats

- ADK 1.0 still ships some agents with `streaming=False` in the Agent
  Card. a2abridge falls back to non-streaming `SendMessage` automatically
  when `card.capabilities.streaming` is false.
- ADK uses `protocolVersion: "1.0"` per spec. If you see version errors
  in `a2abridge doctor`, both sides need to be on protocol 1.0+.
