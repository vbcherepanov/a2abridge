"""Interop check: a2abridge v4 <-> official A2A Python SDK (a2a-sdk 1.1.0).

Direction A: official Python client -> real `a2abridge bridge` (JSON-RPC and HTTP+JSON,
blocking and streaming, then GetTask). The bridge's local agent is played by this
script over MCP stdio: it reads a2a_inbox and answers with a2a_complete_task.

Direction B: a2abridge (MCP a2a_send_message / a2a_send_streaming) -> official Python
helloworld sample from a2a-samples 6603ba3, unmodified.
"""

import asyncio
import json
import os
import socket
import sys
import time
from pathlib import Path
from urllib.parse import urlsplit

import httpx
from google.protobuf.json_format import MessageToDict

from a2a.client import A2ACardResolver, ClientConfig, create_client
from a2a.helpers import new_text_message
from a2a.types import GetTaskRequest, Role, SendMessageRequest

BRIDGE_BIN = os.environ['BRIDGE_BIN']
WORK = Path(os.environ['WORK'])
HELLOWORLD_DIR = Path(os.environ['HELLOWORLD_DIR'])
PYTHON = sys.executable
DIRECTORY_ADDR = '127.0.0.1:17777'
HELLOWORLD_URL = 'http://127.0.0.1:9999'
STARTUP_TIMEOUT = 20.0
CALL_TIMEOUT = 30.0
POLL_INTERVAL = 0.2

results: list[tuple[str, bool, str]] = []


def record(name: str, ok: bool, detail: str) -> None:
    results.append((name, ok, detail))
    print(f'[{"PASS" if ok else "FAIL"}] {name}: {detail}', flush=True)


def wait_port(addr: str) -> None:
    host, port = addr.rsplit(':', 1)
    deadline = time.monotonic() + STARTUP_TIMEOUT
    while time.monotonic() < deadline:
        with socket.socket() as s:
            if s.connect_ex((host, int(port))) == 0:
                return
        time.sleep(POLL_INTERVAL)
    raise RuntimeError(f'{addr} did not start listening')


class MCP:
    def __init__(self, proc: asyncio.subprocess.Process) -> None:
        self.proc = proc
        self.next_id = 0

    async def request(self, method: str, params: dict) -> dict:
        self.next_id += 1
        rid = self.next_id
        line = json.dumps({'jsonrpc': '2.0', 'id': rid, 'method': method, 'params': params})
        self.proc.stdin.write(line.encode() + b'\n')
        await self.proc.stdin.drain()
        while True:
            raw = await asyncio.wait_for(self.proc.stdout.readline(), CALL_TIMEOUT + 10)
            if not raw:
                raise RuntimeError('bridge closed MCP stdout')
            msg = json.loads(raw)
            if msg.get('id') == rid:
                if 'error' in msg:
                    raise RuntimeError(f'MCP {method} error: {msg["error"]}')
                return msg['result']

    async def notify(self, method: str) -> None:
        self.proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': method}).encode() + b'\n')
        await self.proc.stdin.drain()

    async def tool(self, name: str, **args) -> tuple[bool, str]:
        res = await self.request('tools/call', {'name': name, 'arguments': args})
        text = ''.join(c.get('text', '') for c in res.get('content', []))
        return not res.get('isError', False), text


def payload(resp) -> tuple[str, dict]:
    fields = resp.ListFields()
    if not fields:
        return 'empty', {}
    desc, value = fields[0]
    return desc.name, MessageToDict(value)


async def answer_from_inbox(mcp: MCP, query: str, reply: str) -> str:
    deadline = time.monotonic() + CALL_TIMEOUT
    while time.monotonic() < deadline:
        ok, text = await mcp.tool('a2a_inbox', peek=True)
        if ok and text.strip() not in ('', '[]'):
            for entry in json.loads(text):
                if entry.get('text') == query:
                    done, out = await mcp.tool('a2a_complete_task', task_id=entry['taskId'], text=reply)
                    if not done:
                        raise RuntimeError(f'a2a_complete_task failed: {out}')
                    await mcp.tool('a2a_inbox', peek=False)
                    return entry['taskId']
        await asyncio.sleep(POLL_INTERVAL)
    raise RuntimeError(f'message {query!r} never reached the bridge inbox')


async def python_to_bridge(mcp: MCP, base_url: str, binding: str, streaming: bool) -> None:
    label = f'A python->bridge {binding} {"streaming" if streaming else "blocking"}'
    query = f'interop {binding} streaming={streaming}'
    reply = f'bridge reply to: {query}'
    try:
        async with httpx.AsyncClient(timeout=CALL_TIMEOUT) as http:
            card = await A2ACardResolver(httpx_client=http, base_url=base_url).get_agent_card()
            client = await create_client(
                agent=card,
                client_config=ClientConfig(
                    streaming=streaming,
                    httpx_client=http,
                    supported_protocol_bindings=[binding],
                ),
            )
            request = SendMessageRequest(message=new_text_message(query, role=Role.ROLE_USER))

            async def collect() -> list[tuple[str, dict]]:
                return [payload(r) async for r in client.send_message(request)]

            collector = asyncio.create_task(collect())
            task_id = await answer_from_inbox(mcp, query, reply)
            events = await asyncio.wait_for(collector, CALL_TIMEOUT)
            for kind, body in events:
                print(f'    {kind}: {json.dumps(body)[:300]}')

            task = await client.get_task(GetTaskRequest(id=task_id))
            state = MessageToDict(task.status).get('state')
            texts = [p.text for a in task.artifacts for p in a.parts if p.text]
            await client.close()
        ok = state == 'TASK_STATE_COMPLETED' and reply in texts
        record(label, ok, f'events={[k for k, _ in events]} GetTask state={state} artifacts={texts}')
    except Exception as exc:  # the harness reports every scenario, even a failing one
        record(label, False, f'{type(exc).__name__}: {exc}')


async def bridge_to_python(mcp: MCP) -> None:
    query = 'Hi from a2abridge v4'
    ok, text = await mcp.tool('a2a_send_message', peer_url=HELLOWORLD_URL, text=query, blocking=True)
    print(f'    a2a_send_message -> {text[:600]}')
    record('B bridge->python a2a_send_message blocking', ok and 'Hello, World!' in text and 'TASK_STATE_COMPLETED' in text, text[:160])

    ok, text = await mcp.tool('a2a_send_streaming', peer_url=HELLOWORLD_URL, text=query, timeout_s=CALL_TIMEOUT)
    print(f'    a2a_send_streaming -> {text[:600]}')
    record('B bridge->python a2a_send_streaming', ok and 'Hello, World!' in text and 'TASK_STATE_COMPLETED' in text, text[:160])


async def main() -> int:
    WORK.mkdir(parents=True, exist_ok=True)
    procs: list[asyncio.subprocess.Process] = []
    logs = []
    try:
        hw_log = open(WORK / 'helloworld.log', 'wb')
        dir_log = open(WORK / 'directory.log', 'wb')
        bridge_err = open(WORK / 'bridge-stderr.log', 'wb')
        logs += [hw_log, dir_log, bridge_err]

        procs.append(await asyncio.create_subprocess_exec(
            PYTHON, '__main__.py', cwd=HELLOWORLD_DIR, stdout=hw_log, stderr=hw_log))
        procs.append(await asyncio.create_subprocess_exec(
            BRIDGE_BIN, 'directory', '--addr', DIRECTORY_ADDR, stdout=dir_log, stderr=dir_log))
        wait_port('127.0.0.1:9999')
        wait_port(DIRECTORY_ADDR)

        bridge = await asyncio.create_subprocess_exec(
            BRIDGE_BIN, 'bridge',
            '--directory', f'http://{DIRECTORY_ADDR}',
            '--name', 'interop-bridge',
            '--state-dir', str(WORK / 'state'),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=bridge_err)
        procs.append(bridge)
        mcp = MCP(bridge)
        await mcp.request('initialize', {
            'protocolVersion': '2025-06-18',
            'capabilities': {},
            'clientInfo': {'name': 'interop-harness', 'version': '1'},
        })
        await mcp.notify('notifications/initialized')

        ok, text = await mcp.tool('a2a_whoami')
        card = json.loads(text)
        interfaces = card.get('supportedInterfaces', [])
        print('bridge card interfaces:', json.dumps(interfaces))
        first = urlsplit(interfaces[0]['url'])
        base_url = f'{first.scheme}://{first.netloc}'
        bindings = sorted({i['protocolBinding'] for i in interfaces})
        record('bridge card advertises JSONRPC and HTTP+JSON', {'JSONRPC', 'HTTP+JSON'} <= set(bindings), f'{bindings} at {base_url}')

        for binding in ('JSONRPC', 'HTTP+JSON'):
            for streaming in (False, True):
                await python_to_bridge(mcp, base_url, binding, streaming)
        await bridge_to_python(mcp)
    finally:
        for p in reversed(procs):
            if p.returncode is None:
                p.terminate()
                try:
                    await asyncio.wait_for(p.wait(), 5)
                except TimeoutError:
                    p.kill()
        for f in logs:
            f.close()

    failed = [r for r in results if not r[1]]
    print(f'\nSUMMARY: {len(results) - len(failed)} passed, {len(failed)} failed')
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(asyncio.run(main()))
