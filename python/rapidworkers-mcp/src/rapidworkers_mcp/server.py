"""stdio MCP server that relays every request to the hosted RapidWorkers MCP server.

MCP clients that launch local servers (Claude Desktop, Cursor, Windsurf, Zed,
Cline...) talk to this process over stdin/stdout. Each JSON-RPC request is sent
unchanged to https://www.rapidworkers.io/api/mcp/ with the API key, and the
answer is written back, so the tools, instructions, prompts and resources are
always exactly the live ones.

Without RAPIDWORKERS_API_KEY the server still starts and lists the tools from a
bundled snapshot (so registries and inspectors can see them); every tool call
then explains how to create a key.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
import threading
from importlib import resources
from typing import Any

import httpx

from . import __version__

API_KEY_ENV = "RAPIDWORKERS_API_KEY"
URL_ENV = "RAPIDWORKERS_MCP_URL"
API_KEYS_URL = "https://www.rapidworkers.io/dashboard/api-management/"
KNOWN_PROTOCOL_VERSIONS = ("2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05")
DEFAULT_PROTOCOL_VERSION = "2025-06-18"
# Answered before anything ran upstream, so resending is always safe
RETRY_STATUS = {429, 503}
MAX_RETRIES = 2

PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INTERNAL_ERROR = -32603
UPSTREAM_ERROR = -32000

NO_KEY_MESSAGE = (
    f"RAPIDWORKERS_API_KEY is not set. Create an API key at {API_KEYS_URL} and add it to this MCP "
    f"server's environment (for example \"env\": {{\"{API_KEY_ENV}\": \"...\"}}), then restart the client."
)


def log(message: str) -> None:
    print(f"[rapidworkers-mcp] {message}", file=sys.stderr, flush=True)


def load_spec() -> dict[str, Any]:
    return json.loads(resources.files(__package__).joinpath("tools.json").read_text(encoding="utf-8"))


def error_response(request_id: Any, code: int, message: str, data: Any = None) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return {"jsonrpc": "2.0", "id": request_id, "error": error}


def result_response(request_id: Any, result: Any) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def parse_upstream(response: httpx.Response, request_id: Any) -> dict[str, Any] | None:
    """The JSON-RPC message answering request_id, from a JSON or SSE body."""
    if "text/event-stream" in response.headers.get("content-type", ""):
        for line in response.text.splitlines():
            if line.startswith("data:"):
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


class Proxy:
    def __init__(self, api_key: str, url: str, spec: dict[str, Any], http: httpx.AsyncClient | None = None):
        self.api_key = api_key
        self.url = url
        self.spec = spec
        self.protocol_version: str | None = None
        self.http = http or httpx.AsyncClient(timeout=httpx.Timeout(120.0, connect=15.0))

    async def handle(self, message: Any) -> dict[str, Any] | None:
        """The answer to one client message, or None for notifications."""
        if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
            return error_response(None, INVALID_REQUEST, "Invalid JSON-RPC message.")
        method = message.get("method")
        if not isinstance(method, str):
            return None  # a response to a server request; this server sends none
        if "id" not in message:
            return None  # notifications (initialized, cancelled...) need no answer
        request_id = message["id"]
        if method == "ping":
            return result_response(request_id, {})
        if not self.api_key:
            return self.offline(request_id, method, message.get("params") or {})
        return await self.forward(message)

    async def forward(self, message: dict[str, Any]) -> dict[str, Any]:
        request_id = message["id"]
        headers = {
            "X-API-Key": self.api_key,
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "User-Agent": f"rapidworkers-mcp-python/{__version__}",
        }
        if self.protocol_version:
            headers["MCP-Protocol-Version"] = self.protocol_version
        attempt = 0
        while True:
            try:
                response = await self.http.post(self.url, json=message, headers=headers)
            except httpx.HTTPError as exc:
                note = ""
                if message.get("method") == "tools/call":
                    note = " If this call moves money, retry it with the same idempotency_key."
                return error_response(request_id, UPSTREAM_ERROR, f"Could not reach RapidWorkers ({type(exc).__name__}).{note}")
            if response.status_code in RETRY_STATUS and attempt < MAX_RETRIES:
                attempt += 1
                await asyncio.sleep(retry_delay(response, attempt))
                continue
            break

        answer = parse_upstream(response, request_id)
        if response.status_code >= 400 or answer is None:
            upstream = (answer or {}).get("error") or {}
            text = upstream.get("message") or f"RapidWorkers answered HTTP {response.status_code}."
            if response.status_code == 401:
                text = f"{text} Check {API_KEY_ENV} or create a new key at {API_KEYS_URL}"
            return error_response(request_id, upstream.get("code") or UPSTREAM_ERROR, text, upstream.get("data"))
        if message.get("method") == "initialize" and isinstance(answer.get("result"), dict):
            version = answer["result"].get("protocolVersion")
            self.protocol_version = version if isinstance(version, str) else None
        answer["id"] = request_id
        return answer

    def offline(self, request_id: Any, method: str, params: dict[str, Any]) -> dict[str, Any]:
        """Answers without an API key: enough to start and list tools."""
        server = self.spec["server"]
        if method == "initialize":
            requested = params.get("protocolVersion")
            version = requested if requested in KNOWN_PROTOCOL_VERSIONS else DEFAULT_PROTOCOL_VERSION
            return result_response(
                request_id,
                {
                    "protocolVersion": version,
                    "capabilities": {"tools": {"listChanged": False}},
                    "serverInfo": {"name": server["name"], "title": server["title"], "version": __version__},
                    "instructions": f"{server['instructions']}\n\n{NO_KEY_MESSAGE}",
                },
            )
        if method == "tools/list":
            return result_response(request_id, {"tools": self.spec["tools"]})
        if method == "tools/call":
            return result_response(request_id, {"content": [{"type": "text", "text": NO_KEY_MESSAGE}], "isError": True})
        if method == "resources/list":
            return result_response(request_id, {"resources": []})
        if method == "resources/templates/list":
            return result_response(request_id, {"resourceTemplates": []})
        if method == "prompts/list":
            return result_response(request_id, {"prompts": []})
        return error_response(request_id, METHOD_NOT_FOUND, f"{method} needs {API_KEY_ENV}. {NO_KEY_MESSAGE}")


def retry_delay(response: httpx.Response, attempt: int) -> float:
    try:
        return min(max(float(response.headers.get("retry-after", "")), 0.0), 30.0)
    except ValueError:
        return min(0.5 * 2**attempt, 30.0)


def write(message: dict[str, Any], lock: threading.Lock) -> None:
    line = json.dumps(message, separators=(",", ":"), ensure_ascii=False) + "\n"
    with lock:
        sys.stdout.buffer.write(line.encode("utf-8"))
        sys.stdout.buffer.flush()


async def serve(proxy: Proxy) -> None:
    loop = asyncio.get_running_loop()
    queue: asyncio.Queue[bytes | None] = asyncio.Queue()
    lock = threading.Lock()

    def read_stdin() -> None:
        # A thread works for pipes on every platform, unlike connect_read_pipe on Windows
        for line in sys.stdin.buffer:
            loop.call_soon_threadsafe(queue.put_nowait, line)
        loop.call_soon_threadsafe(queue.put_nowait, None)

    threading.Thread(target=read_stdin, daemon=True).start()
    pending: set[asyncio.Task[None]] = set()

    async def answer(raw: bytes) -> None:
        try:
            message = json.loads(raw)
        except ValueError:
            write(error_response(None, PARSE_ERROR, "Parse error."), lock)
            return
        messages = message if isinstance(message, list) else [message]
        for item in messages:
            try:
                reply = await proxy.handle(item)
            except Exception as exc:  # never let one request kill the server
                log(f"internal error: {exc!r}")
                request_id = item.get("id") if isinstance(item, dict) else None
                reply = error_response(request_id, INTERNAL_ERROR, "Internal error in rapidworkers-mcp.")
            if reply is not None:
                write(reply, lock)

    while True:
        raw = await queue.get()
        if raw is None:
            break
        if not raw.strip():
            continue
        task = asyncio.create_task(answer(raw))
        pending.add(task)
        task.add_done_callback(pending.discard)
    if pending:
        await asyncio.gather(*pending, return_exceptions=True)
    await proxy.http.aclose()


def main() -> None:
    if any(arg in ("-V", "--version") for arg in sys.argv[1:]):
        print(f"rapidworkers-mcp {__version__}")
        return
    if any(arg in ("-h", "--help") for arg in sys.argv[1:]):
        print(__doc__)
        print(f"Environment: {API_KEY_ENV} (required for tool calls), {URL_ENV} (optional endpoint override).")
        return
    spec = load_spec()
    api_key = os.environ.get(API_KEY_ENV, "").strip()
    url = os.environ.get(URL_ENV, "").strip() or spec["server"]["url"]
    if not api_key:
        log(f"{API_KEY_ENV} is not set: tools are listed but calls will ask for a key ({API_KEYS_URL}).")
    try:
        asyncio.run(serve(Proxy(api_key, url, spec)))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
