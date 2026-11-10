"""HTTP clients for the hosted RapidWorkers MCP server.

Each tool call is one JSON-RPC ``tools/call`` POST to the stateless MCP
endpoint, so the SDK gets exactly the validation, cost quotes, safeguards and
error messages the server gives AI agents.
"""

from __future__ import annotations

import asyncio
import itertools
import json
import os
import time
import uuid
from typing import Any, Mapping

import httpx

from ._generated import SERVER_URL, TOOL_META, AsyncTools, SyncTools
from ._version import __version__
from .errors import (
    APIError,
    AuthenticationError,
    ConfigurationError,
    APIConnectionError,
    RateLimitError,
    ServiceUnavailableError,
    ToolError,
)

API_KEY_ENV = "RAPIDWORKERS_API_KEY"
BASE_URL_ENV = "RAPIDWORKERS_MCP_URL"
API_KEYS_URL = "https://www.rapidworkers.io/dashboard/api-management/"
PROTOCOL_VERSION = "2025-06-18"
DEFAULT_TIMEOUT = 60.0
DEFAULT_MAX_RETRIES = 2
# Answered before any tool ran: safe to resend for every call
RETRY_ALWAYS = {429, 503}
# A gateway error may come after the tool ran: resend only retry-safe calls
RETRY_IF_SAFE = {502, 504}


def _resolve_api_key(api_key: str | None) -> str:
    key = api_key or os.environ.get(API_KEY_ENV, "")
    key = key.strip()
    if not key:
        raise ConfigurationError(
            f"No RapidWorkers API key. Pass api_key=... or set {API_KEY_ENV}. Create a key at {API_KEYS_URL}"
        )
    return key


def _headers(api_key: str) -> dict[str, str]:
    return {
        "X-API-Key": api_key,
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "MCP-Protocol-Version": PROTOCOL_VERSION,
        "User-Agent": f"rapidworkers-python/{__version__}",
    }


def _prepare(name: str, arguments: Mapping[str, Any]) -> dict[str, Any]:
    """Drop unset arguments and add an idempotency key where the tool takes one."""
    args = {k: (list(v) if isinstance(v, tuple) else v) for k, v in arguments.items() if v is not None}
    meta = TOOL_META.get(name)
    if meta and meta["idempotency_key"] and "idempotency_key" not in args:
        args["idempotency_key"] = f"sdk-{uuid.uuid4().hex}"
    return args


def _retry_after(response: httpx.Response) -> float | None:
    value = response.headers.get("retry-after")
    try:
        return max(0.0, float(value)) if value else None
    except ValueError:
        return None


def _backoff(attempt: int, response: httpx.Response | None) -> float:
    hinted = _retry_after(response) if response is not None else None
    return min(hinted if hinted is not None else 0.5 * (2**attempt), 30.0)


def _rpc_message(response: httpx.Response, request_id: int) -> dict[str, Any] | None:
    """The JSON-RPC answer to request_id from a JSON or SSE response body."""
    content_type = response.headers.get("content-type", "")
    if "text/event-stream" in content_type:
        for line in response.text.splitlines():
            if not line.startswith("data:"):
                continue
            try:
                message = json.loads(line[5:].strip())
            except ValueError:
                continue
            if isinstance(message, dict) and message.get("id") == request_id:
                return message
        return None
    try:
        message = response.json()
    except ValueError:
        return None
    if isinstance(message, list):
        message = next((m for m in message if isinstance(m, dict) and m.get("id") == request_id), None)
    return message if isinstance(message, dict) else None


def _http_error(response: httpx.Response, message: dict[str, Any] | None) -> APIError:
    error = (message or {}).get("error") or {}
    text = error.get("message") or response.text[:500] or f"HTTP {response.status_code}"
    kwargs: dict[str, Any] = {"status_code": response.status_code, "code": error.get("code"), "data": error.get("data")}
    if response.status_code == 401:
        return AuthenticationError(f"{text} Check your API key or create one at {API_KEYS_URL}", **kwargs)
    if response.status_code == 429:
        return RateLimitError(text, retry_after=_retry_after(response), **kwargs)
    if response.status_code in (502, 503, 504):
        return ServiceUnavailableError(text, **kwargs)
    return APIError(text, **kwargs)


def _unwrap(method: str, params: Mapping[str, Any], response: httpx.Response, request_id: int) -> Any:
    message = _rpc_message(response, request_id)
    if response.status_code >= 400:
        raise _http_error(response, message)
    if message is None:
        raise APIError("Unreadable answer from RapidWorkers.", status_code=response.status_code)
    if "error" in message:
        error = message["error"] or {}
        raise APIError(
            error.get("message") or "JSON-RPC error",
            status_code=response.status_code,
            code=error.get("code"),
            data=error.get("data"),
        )
    result = message.get("result")
    if method != "tools/call":
        return result
    return _tool_result(str(params.get("name")), result or {})


def _tool_result(tool: str, result: Mapping[str, Any]) -> dict[str, Any]:
    structured = result.get("structuredContent")
    text = "\n".join(
        block.get("text", "") for block in result.get("content") or [] if isinstance(block, dict) and block.get("type") == "text"
    )
    if result.get("isError"):
        details = structured.get("error") if isinstance(structured, dict) else None
        details = dict(details) if isinstance(details, dict) else {}
        message = details.pop("message", None) or text or "The tool call failed."
        raise ToolError(tool, message, details)
    if isinstance(structured, dict):
        return structured
    # Text-only answers carry a one-line summary, a blank line and the JSON
    _, _, payload = text.partition("\n\n")
    try:
        data = json.loads(payload or text)
    except ValueError:
        return {"text": text}
    return data if isinstance(data, dict) else {"data": data}


class _Base:
    def __init__(
        self,
        api_key: str | None = None,
        *,
        base_url: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        max_retries: int = DEFAULT_MAX_RETRIES,
    ) -> None:
        self.api_key = _resolve_api_key(api_key)
        self.base_url = base_url or os.environ.get(BASE_URL_ENV) or SERVER_URL
        self.timeout = timeout
        self.max_retries = max(0, max_retries)
        self._ids = itertools.count(1)

    def _payload(self, method: str, params: Mapping[str, Any]) -> tuple[int, dict[str, Any]]:
        request_id = next(self._ids)
        return request_id, {"jsonrpc": "2.0", "id": request_id, "method": method, "params": dict(params)}

    def _can_retry(self, method: str, params: Mapping[str, Any]) -> bool:
        if method != "tools/call":
            return True
        meta = TOOL_META.get(str(params.get("name")))
        return bool(meta and meta["retry"])

    @staticmethod
    def _should_retry(status: int, retry_safe: bool) -> bool:
        return status in RETRY_ALWAYS or (retry_safe and status in RETRY_IF_SAFE)

    def __repr__(self) -> str:
        return f"{type(self).__name__}(base_url={self.base_url!r})"


class RapidWorkers(_Base, SyncTools):
    """Synchronous RapidWorkers client.

    >>> from rapidworkers import RapidWorkers
    >>> rw = RapidWorkers()  # reads RAPIDWORKERS_API_KEY
    >>> rw.get_account()["balance"]

    Every tool of the RapidWorkers MCP server is a method. Calls that move
    money get an idempotency key automatically, and a timed-out call is retried
    with the same key, so it never happens twice.
    """

    def __init__(self, api_key: str | None = None, *, http_client: httpx.Client | None = None, **kwargs: Any) -> None:
        super().__init__(api_key, **kwargs)
        self._owns_http = http_client is None
        self._http = http_client or httpx.Client(timeout=self.timeout)

    def close(self) -> None:
        if self._owns_http:
            self._http.close()

    def __enter__(self) -> RapidWorkers:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def request(self, method: str, params: Mapping[str, Any] | None = None) -> Any:
        """Send one JSON-RPC request (e.g. ``tools/list``) and return its result."""
        params = params or {}
        retry = self._can_retry(method, params)
        attempt = 0
        while True:
            request_id, payload = self._payload(method, params)
            try:
                response = self._http.post(self.base_url, json=payload, headers=_headers(self.api_key))
            except httpx.TransportError as exc:
                if retry and attempt < self.max_retries:
                    time.sleep(_backoff(attempt, None))
                    attempt += 1
                    continue
                raise APIConnectionError(f"Could not reach RapidWorkers: {exc}") from exc
            if self._should_retry(response.status_code, retry) and attempt < self.max_retries:
                time.sleep(_backoff(attempt, response))
                attempt += 1
                continue
            return _unwrap(method, params, response, request_id)

    def _call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        return self.call_tool(name, arguments)

    def call_tool(self, name: str, arguments: Mapping[str, Any] | None = None) -> dict[str, Any]:
        """Call any tool by name. Raises ToolError when the tool refuses the call."""
        return self.request("tools/call", {"name": name, "arguments": _prepare(name, arguments or {})})

    def list_tools(self) -> list[dict[str, Any]]:
        """The live tool definitions (name, description, inputSchema, annotations)."""
        return list((self.request("tools/list") or {}).get("tools") or [])


class AsyncRapidWorkers(_Base, AsyncTools):
    """Asynchronous RapidWorkers client; same methods as RapidWorkers, awaitable."""

    def __init__(self, api_key: str | None = None, *, http_client: httpx.AsyncClient | None = None, **kwargs: Any) -> None:
        super().__init__(api_key, **kwargs)
        self._owns_http = http_client is None
        self._http = http_client or httpx.AsyncClient(timeout=self.timeout)

    async def aclose(self) -> None:
        if self._owns_http:
            await self._http.aclose()

    async def __aenter__(self) -> AsyncRapidWorkers:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def request(self, method: str, params: Mapping[str, Any] | None = None) -> Any:
        """Send one JSON-RPC request (e.g. ``tools/list``) and return its result."""
        params = params or {}
        retry = self._can_retry(method, params)
        attempt = 0
        while True:
            request_id, payload = self._payload(method, params)
            try:
                response = await self._http.post(self.base_url, json=payload, headers=_headers(self.api_key))
            except httpx.TransportError as exc:
                if retry and attempt < self.max_retries:
                    await asyncio.sleep(_backoff(attempt, None))
                    attempt += 1
                    continue
                raise APIConnectionError(f"Could not reach RapidWorkers: {exc}") from exc
            if self._should_retry(response.status_code, retry) and attempt < self.max_retries:
                await asyncio.sleep(_backoff(attempt, response))
                attempt += 1
                continue
            return _unwrap(method, params, response, request_id)

    async def _call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        return await self.call_tool(name, arguments)

    async def call_tool(self, name: str, arguments: Mapping[str, Any] | None = None) -> dict[str, Any]:
        """Call any tool by name. Raises ToolError when the tool refuses the call."""
        return await self.request("tools/call", {"name": name, "arguments": _prepare(name, arguments or {})})

    async def list_tools(self) -> list[dict[str, Any]]:
        """The live tool definitions (name, description, inputSchema, annotations)."""
        return list(((await self.request("tools/list")) or {}).get("tools") or [])
