"""Errors raised by the RapidWorkers client."""

from __future__ import annotations

from typing import Any


class RapidWorkersError(Exception):
    """Base class for every error this package raises."""


class ConfigurationError(RapidWorkersError):
    """The client is missing something it needs, such as an API key."""


class APIError(RapidWorkersError):
    """The server answered with an HTTP or JSON-RPC error."""

    def __init__(self, message: str, *, status_code: int | None = None, code: int | None = None, data: Any = None):
        super().__init__(message)
        self.message = message
        self.status_code = status_code
        self.code = code
        self.data = data


class AuthenticationError(APIError):
    """The API key is missing, invalid, revoked, or the account cannot use it (HTTP 401)."""


class RateLimitError(APIError):
    """Too many requests for this API key (HTTP 429)."""

    def __init__(self, message: str, *, retry_after: float | None = None, **kwargs: Any):
        super().__init__(message, **kwargs)
        self.retry_after = retry_after


class ServiceUnavailableError(APIError):
    """RapidWorkers is temporarily unavailable (HTTP 502, 503 or 504)."""


class APIConnectionError(RapidWorkersError):
    """The request did not reach RapidWorkers or timed out with no answer."""


class ToolError(RapidWorkersError):
    """The tool ran and refused the call. ``message`` says how to fix it.

    Typical causes: a validation error, an insufficient balance, a stale
    ``quote_id`` or a job in the wrong state. ``details`` holds the structured
    error the server sent (for example ``code``, ``fields`` or ``replayed``).
    """

    def __init__(self, tool: str, message: str, details: dict[str, Any] | None = None):
        super().__init__(message)
        self.tool = tool
        self.message = message
        self.details = details or {}

    @property
    def code(self) -> str | None:
        value = self.details.get("code")
        return value if isinstance(value, str) else None
