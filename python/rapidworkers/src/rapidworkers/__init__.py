"""Official Python client for RapidWorkers: hire real people for microtasks,
app testing, surveys and AI data collection (photos, audio, video, text)."""

from ._client import API_KEY_ENV, API_KEYS_URL, AsyncRapidWorkers, RapidWorkers
from ._generated import SERVER_URL
from ._version import __version__
from .errors import (
    APIConnectionError,
    APIError,
    AuthenticationError,
    ConfigurationError,
    RapidWorkersError,
    RateLimitError,
    ServiceUnavailableError,
    ToolError,
)

__all__ = [
    "API_KEY_ENV",
    "API_KEYS_URL",
    "SERVER_URL",
    "APIConnectionError",
    "APIError",
    "AsyncRapidWorkers",
    "AuthenticationError",
    "ConfigurationError",
    "RapidWorkers",
    "RapidWorkersError",
    "RateLimitError",
    "ServiceUnavailableError",
    "ToolError",
    "__version__",
]
