import json

import httpx
import pytest
import respx

import rapidworkers
from rapidworkers import (
    AsyncRapidWorkers,
    AuthenticationError,
    ConfigurationError,
    RapidWorkers,
    ToolError,
)

URL = "https://mcp.test/api/mcp/"


def ok(result, request_id=None):
    def handler(request):
        body = json.loads(request.content)
        return httpx.Response(200, json={"jsonrpc": "2.0", "id": request_id or body["id"], "result": result})

    return handler


def structured(data):
    return {"content": [{"type": "text", "text": "summary\n\n" + json.dumps(data)}], "structuredContent": data}


@pytest.fixture
def client():
    with RapidWorkers(api_key="k" * 64, base_url=URL, max_retries=2) as rw:
        yield rw


@pytest.fixture(autouse=True)
def no_sleep(monkeypatch):
    monkeypatch.setattr("rapidworkers._client.time.sleep", lambda s: None)


def test_requires_api_key(monkeypatch):
    monkeypatch.delenv("RAPIDWORKERS_API_KEY", raising=False)
    with pytest.raises(ConfigurationError, match="api-management"):
        RapidWorkers()


def test_reads_api_key_from_env(monkeypatch):
    monkeypatch.setenv("RAPIDWORKERS_API_KEY", "envkey" * 5)
    assert RapidWorkers().api_key == "envkey" * 5


@respx.mock
def test_tool_call_sends_jsonrpc_and_returns_structured_content(client):
    route = respx.post(URL).mock(side_effect=ok(structured({"balance": "12.50"})))
    assert client.get_account() == {"balance": "12.50"}
    request = route.calls.last.request
    body = json.loads(request.content)
    assert body["method"] == "tools/call"
    assert body["params"] == {"name": "get_account", "arguments": {}}
    assert request.headers["X-API-Key"] == "k" * 64
    assert "text/event-stream" in request.headers["Accept"]


@respx.mock
def test_unset_arguments_are_dropped(client):
    route = respx.post(URL).mock(side_effect=ok(structured({"jobs": []})))
    client.list_jobs(kind="ai_task")
    args = json.loads(route.calls.last.request.content)["params"]["arguments"]
    assert args == {"kind": "ai_task"}


@respx.mock
def test_money_calls_get_an_idempotency_key_reused_on_retry(client):
    route = respx.post(URL).mock(
        side_effect=[httpx.ConnectTimeout("boom"), httpx.Response(504), ok(structured({"approved": True}))]
    )
    client.approve_submission(job_id="0" * 8 + "-0000-0000-0000-" + "0" * 12, submission_id=5)
    keys = {json.loads(c.request.content)["params"]["arguments"]["idempotency_key"] for c in route.calls[2:]}
    sent = [json.loads(c.request.content) for c in respx.calls if c.request.content]
    keys = {m["params"]["arguments"]["idempotency_key"] for m in sent}
    assert len(route.calls) == 3
    assert len(keys) == 1 and next(iter(keys)).startswith("sdk-")


@respx.mock
def test_explicit_idempotency_key_is_kept(client):
    route = respx.post(URL).mock(side_effect=ok(structured({"ok": True})))
    client.cancel_job(job_id="x", confirm=True, idempotency_key="mine-1")
    assert json.loads(route.calls.last.request.content)["params"]["arguments"]["idempotency_key"] == "mine-1"


@respx.mock
def test_non_idempotent_write_is_not_retried_after_gateway_error(client):
    route = respx.post(URL).mock(return_value=httpx.Response(504, text="gateway timeout"))
    with pytest.raises(rapidworkers.ServiceUnavailableError):
        client.get_deposit_link(amount=20)
    assert len(route.calls) == 1


@respx.mock
def test_429_is_retried_for_any_tool(client):
    route = respx.post(URL).mock(
        side_effect=[httpx.Response(429, headers={"Retry-After": "1"}), ok(structured({"url": "https://pay"}))]
    )
    assert client.get_deposit_link(amount=20) == {"url": "https://pay"}
    assert len(route.calls) == 2


@respx.mock
def test_tool_error_raises_with_details(client):
    result = {
        "content": [{"type": "text", "text": "Balance too low."}],
        "structuredContent": {"error": {"message": "Balance too low.", "code": "insufficient_balance"}},
        "isError": True,
    }
    respx.post(URL).mock(side_effect=ok(result))
    with pytest.raises(ToolError) as info:
        client.preview_job_cost(pay_rate=0.05, quantity=10)
    assert info.value.message == "Balance too low."
    assert info.value.code == "insufficient_balance"
    assert info.value.tool == "preview_job_cost"


@respx.mock
def test_401_raises_authentication_error(client):
    respx.post(URL).mock(
        return_value=httpx.Response(401, json={"jsonrpc": "2.0", "id": None, "error": {"code": -32001, "message": "Invalid token."}})
    )
    with pytest.raises(AuthenticationError, match="Invalid token"):
        client.get_account()


@respx.mock
def test_sse_answer_is_parsed(client):
    def handler(request):
        body = json.loads(request.content)
        message = {"jsonrpc": "2.0", "id": body["id"], "result": structured({"n": 1})}
        return httpx.Response(200, text=f"event: message\ndata: {json.dumps(message)}\n\n", headers={"content-type": "text/event-stream"})

    respx.post(URL).mock(side_effect=handler)
    assert client.list_blocked_workers() == {"n": 1}


@respx.mock
def test_text_only_result_is_parsed(client):
    respx.post(URL).mock(side_effect=ok({"content": [{"type": "text", "text": "One job.\n\n{\"jobs\": [1]}"}]}))
    assert client.list_jobs() == {"jobs": [1]}


@respx.mock
def test_list_tools(client):
    respx.post(URL).mock(side_effect=ok({"tools": [{"name": "get_account"}]}))
    assert client.list_tools() == [{"name": "get_account"}]


@respx.mock
async def test_async_client():
    respx.post(URL).mock(side_effect=ok(structured({"balance": "1.00"})))
    async with AsyncRapidWorkers(api_key="k" * 64, base_url=URL) as rw:
        assert await rw.get_account() == {"balance": "1.00"}


def test_every_spec_tool_has_a_method():
    spec = json.load(open(__file__.rsplit("/python/", 1)[0] + "/spec/tools.json"))
    for tool in spec["tools"]:
        assert callable(getattr(RapidWorkers, tool["name"]))
        assert callable(getattr(AsyncRapidWorkers, tool["name"]))
