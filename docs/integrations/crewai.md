# CrewAI ⇄ a2abridge

CrewAI added native A2A 1.0 support in 0.95.x via `crewai.tools.A2AAgentTool`.
A whole `Crew` can be exposed as a single A2A peer; tasks dispatched
through the bridge are routed into Crew's task queue.

## Expose a Crew as an A2A peer

```python
from crewai import Crew, Agent, Task
from crewai.adapters.a2a import expose_as_a2a
import requests

crew = Crew(
    agents=[Agent(role="planner", ...), Agent(role="coder", ...)],
    tasks=[Task(description="...")],
)

server = expose_as_a2a(
    crew,
    name="crewai-team",
    skills=["plan", "implement"],
    bind="127.0.0.1:0",
)
server.start()

requests.post(
    "http://127.0.0.1:7777/register",
    json={"url": server.url},
    timeout=2,
)
```

Now any Claude / Codex / Cursor bridge can dispatch a task to the whole
crew with one `a2a_send_message`. The crew's coordinator agent decides
which member handles which subtask.

## Register an a2abridge peer as a CrewAI tool

```python
from crewai.tools.a2a import A2AAgentTool

claude_tool = A2AAgentTool(
    peer_url="http://127.0.0.1:55477",  # a Claude Code bridge
    name="ask_claude",
    description="Delegate code reviews to a Claude Code instance.",
)

agent = Agent(
    role="reviewer-orchestrator",
    tools=[claude_tool],
    ...,
)
```

## Caveats

- CrewAI's coordinator currently picks ONE agent per inbound task — if
  you want fan-out, write a `Task` whose `description` explicitly asks
  for parallel routing.
- The adapter does NOT relay PII-screen warnings. Apply CrewAI's own
  guardrails in addition to a2abridge's outbound regex screen.
