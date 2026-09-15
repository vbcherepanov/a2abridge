# Interop check with the official Python A2A SDK

`interop.py` starts an isolated a2abridge directory on `127.0.0.1:17777`, the
official Python helloworld agent on `127.0.0.1:9999` and a real `a2abridge bridge`
over MCP stdio. It then exercises both directions. Results are in
[docs/conformance.md](../../docs/conformance.md).

Requirements: Go, [uv](https://docs.astral.sh/uv/) and `curl`. Ports 9999 and 17777
must be free.

```sh
go build -o /tmp/a2abridge ./cmd/a2abridge

uv venv /tmp/a2a-interop --python 3.13
VIRTUAL_ENV=/tmp/a2a-interop uv pip install "a2a-sdk[http-server]==1.1.0" uvicorn httpx sse-starlette

mkdir -p /tmp/a2a-helloworld
for f in __main__.py agent_executor.py; do
  curl -fsSL -o /tmp/a2a-helloworld/$f https://raw.githubusercontent.com/a2aproject/a2a-samples/6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3/samples/python/agents/helloworld/$f
done

BRIDGE_BIN=/tmp/a2abridge WORK=/tmp/a2a-interop-run HELLOWORLD_DIR=/tmp/a2a-helloworld \
  /tmp/a2a-interop/bin/python scripts/interop/interop.py
```

The script exits non-zero if any scenario fails. Process logs are written to `WORK`.
