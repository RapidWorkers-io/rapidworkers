import json
import os
import subprocess
import sys

import httpx
import pytest
import respx

from rapidworkers_mcp.server import Proxy, load_spec

URL = "https://mcp.test/api/mcp/"
SPEC = load_spec()


def rpc(method, request_id=1, params=None):
    return {"jsonrpc": "2.0", "id": request_id, "method": method, "params": params or {}}


@pytest.fixture
async def proxy():
    p = Proxy("k" * 64, URL, SPEC)
    yield p
    await p.http.aclose()


@respx.mock
async def test_forwards_request_unchanged_with_api_key(proxy):
    route = respx.post(URL).mock(
        side_effect=lambda r: httpx.Response(200, json={"jsonrpc": "2.0", "id": json.loads(r.content)["id"], "result": {"ok": 1}})
    )
    message = rpc("tools/call", 7, {"name": "get_account", "arguments": {}})
    assert await proxy.handle(message) == {"jsonrpc": "2.0", "id": 7, "result": {"ok": 1}}
    request = route.calls.last.request
    assert json.loads(request.content) == message
    assert request.headers["X-API-Key"] == "k" * 64


@respx.mock
async def test_initialize_sets_protocol_header_for_later_calls(proxy):
    route = respx.post(URL).mock(
        side_effect=lambda r: httpx.Response(
            200, json={"jsonrpc": "2.0", "id": json.loads(r.content)["id"], "result": {"protocolVersion": "2025-06-18"}}
        )
    )
    await proxy.handle(rpc("initialize", 1, {"protocolVersion": "2025-06-18"}))
    await proxy.handle(rpc("tools/list", 2))
    assert route.calls.last.request.headers["MCP-Protocol-Version"] == "2025-06-18"


@respx.mock
async def test_sse_answer(proxy):
    def handler(request):
        body = json.loads(request.content)
        data = json.dumps({"jsonrpc": "2.0", "id": body["id"], "result": {"tools": []}})
        return httpx.Response(200, text=f"event: message\ndata: {data}\n\n", headers={"content-type": "text/event-stream"})

    respx.post(URL).mock(side_effect=handler)
    assert (await proxy.handle(rpc("tools/list", "abc")))["result"] == {"tools": []}


@respx.mock
async def test_401_becomes_jsonrpc_error_with_key_hint(proxy):
    respx.post(URL).mock(
        return_value=httpx.Response(401, json={"jsonrpc": "2.0", "id": None, "error": {"code": -32001, "message": "Invalid token."}})
    )
    answer = await proxy.handle(rpc("tools/list", 3))
    assert answer["id"] == 3
    assert answer["error"]["code"] == -32001
    assert "api-management" in answer["error"]["message"]


@respx.mock
async def test_503_is_retried(proxy, monkeypatch):
    monkeypatch.setattr("rapidworkers_mcp.server.retry_delay", lambda *a: 0)
    route = respx.post(URL).mock(
        side_effect=[httpx.Response(503), httpx.Response(200, json={"jsonrpc": "2.0", "id": 4, "result": {}})]
    )
    assert (await proxy.handle(rpc("tools/list", 4)))["result"] == {}
    assert route.call_count == 2


@respx.mock
async def test_network_error_on_tool_call_mentions_idempotency(proxy):
    respx.post(URL).mock(side_effect=httpx.ConnectTimeout("x"))
    answer = await proxy.handle(rpc("tools/call", 5, {"name": "approve_submission", "arguments": {}}))
    assert "same idempotency_key" in answer["error"]["message"]


async def test_notifications_and_ping_answered_locally(proxy):
    assert await proxy.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}) is None
    assert await proxy.handle(rpc("ping", 9)) == {"jsonrpc": "2.0", "id": 9, "result": {}}


def test_stdio_without_key_lists_tools_and_explains_calls():
    """End to end over real stdio, no network: what registries see."""
    lines = [
        rpc("initialize", 1, {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}}),
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        rpc("tools/list", 2),
        rpc("tools/call", 3, {"name": "get_account", "arguments": {}}),
    ]
    env = {k: v for k, v in os.environ.items() if k != "RAPIDWORKERS_API_KEY"}
    done = subprocess.run(
        [sys.executable, "-m", "rapidworkers_mcp"],
        input="\n".join(json.dumps(m) for m in lines) + "\n",
        capture_output=True, text=True, env=env, timeout=30,
    )
    answers = {a["id"]: a for a in map(json.loads, done.stdout.splitlines())}
    assert set(answers) == {1, 2, 3}
    assert answers[1]["result"]["serverInfo"]["name"] == "rapidworkers"
    assert answers[1]["result"]["protocolVersion"] == "2025-06-18"
    assert len(answers[2]["result"]["tools"]) == len(SPEC["tools"]) == 20
    assert answers[3]["result"]["isError"] is True
    assert "RAPIDWORKERS_API_KEY" in answers[3]["result"]["content"][0]["text"]
    assert "not set" in done.stderr
