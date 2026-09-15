# A2A 1.0 conformance and interoperability

Measured on 2026-09-15 against **a2abridge v4.0.1** (commit `4279c78`), built on
[a2a-go](https://github.com/a2aproject/a2a-go) v2.5.0. These are reproducible
test runs, not a certification.

## Official A2A TCK

- TCK: [a2aproject/a2a-tck](https://github.com/a2aproject/a2a-tck) at commit
  `263b9cfaf16a554bdfb166a7ba5b67716e946349`.
- System under test: `cmd/a2abridge-tck` (build tag `tck`). It serves the bridge's
  own HTTP stack (`NewA2AHTTPHandler`: JSON-RPC and HTTP+JSON bindings, SSE, push
  notifications) with an executor that plays the TCK scenarios. gRPC is not served.

```sh
go build -tags tck -o a2abridge-tck ./cmd/a2abridge-tck
./a2abridge-tck --addr 127.0.0.1:19991 &
cd /path/to/a2a-tck && uv run ./run_tck.py --sut-host http://127.0.0.1:19991
```

| Run | Passed | Failed | Skipped | xfailed |
|---|---:|---:|---:|---:|
| a2abridge v4.0.1 (JSON-RPC, HTTP+JSON) | 157 | 6 | 95 | 7 |
| TCK's generated Python SUT (`sut/a2a-python`, a2a-sdk 1.1.2), same TCK commit, for reference | 198 | 9 | 56 | 2 |

Most skips are the 72 gRPC checks. The reference SUT serves gRPC as well, so the
totals are not directly comparable.

All six failures come from a2a-go v2.5.0 behaviour that a thin wrapper cannot fix
without re-implementing the SDK:

| Failing check | Cause in a2a-go |
|---|---|
| `test_create_push_config[jsonrpc]` (PUSH-CREATE-001) | JSON-RPC params in proto field names (`task_id`) are ignored; proto3 JSON requires accepting them |
| `test_get_task_history_does_not_exceed_limit[jsonrpc]` (CORE-HIST-002) | same: `history_length` is ignored |
| `test_subscribe_first_event_is_task` (STREAM-SUB-001) | `SubscribeToTask` on an `INPUT_REQUIRED` task reports "no active execution" |
| `test_subscribe_terminates_at_terminal_state[jsonrpc]`, `[http_json]` (STREAM-SUB-002) | same root cause |
| `test_cancel_terminal_task_returns_error[http_json]` (CORE-CANCEL-002) | REST maps `TaskNotCancelable` to 400; spec §5.4 says 409. The reference Python SUT fails this check too |

The bridge itself adds what a2a-go leaves out: `A2A-Version` validation, plain errors
for `SubscribeToTask` on unknown or finished tasks, UTC timestamps, Agent Card
caching headers and a request body limit.

## Interoperability with the official Python SDK

[`scripts/interop/interop.py`](../scripts/interop/interop.py) runs a real
`a2abridge bridge` (MCP over stdio, isolated directory on port 17777) against the
official Python [a2a-sdk](https://pypi.org/project/a2a-sdk/) 1.1.0 and the unmodified
helloworld agent from [a2a-samples](https://github.com/a2aproject/a2a-samples) at
commit `6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3`.

| Scenario | Result |
|---|---|
| Bridge Agent Card advertises JSON-RPC and HTTP+JSON | pass |
| Python client → bridge, JSON-RPC, blocking send, then GetTask | pass |
| Python client → bridge, JSON-RPC, streaming send, then GetTask | pass |
| Python client → bridge, HTTP+JSON, blocking send, then GetTask | pass |
| Python client → bridge, HTTP+JSON, streaming send, then GetTask | pass |
| Bridge (`a2a_send_message`, blocking) → Python helloworld | pass |
| Bridge (`a2a_send_streaming`) → Python helloworld | pass |

`SUMMARY: 7 passed, 0 failed` on v4.0.1. Setup is in
[`scripts/interop/README.md`](../scripts/interop/README.md).
