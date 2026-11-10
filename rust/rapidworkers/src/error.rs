//! Errors returned by the RapidWorkers client.

use std::time::Duration;

use serde_json::{Map, Value};

/// A `Result` whose error is [`Error`].
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Everything a RapidWorkers call can fail with.
///
/// [`Error::Tool`] is the one to read first: the tool ran and refused the call, and `message`
/// says how to fix it. The other variants cover configuration and transport.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// The client is missing something it needs, such as an API key.
    #[error("{0}")]
    Configuration(String),

    /// The tool ran and refused the call, for example a validation error, a low balance, an
    /// expired `quote_id` or a job in the wrong state. `message` says how to fix it.
    #[error("{message}")]
    Tool {
        /// The tool that refused the call.
        tool: String,
        /// What went wrong and how to fix the call.
        message: String,
        /// The structured error the server sent, without `message` (for example `code`,
        /// `fields` or `replayed`).
        details: Box<Map<String, Value>>,
    },

    /// The API key is missing, invalid, revoked, or the account cannot use it (HTTP 401).
    #[error("{message}")]
    Authentication {
        /// HTTP status code.
        status: u16,
        /// JSON-RPC error code, when the server sent one.
        code: Option<i64>,
        /// The server's message, with a hint about API keys.
        message: String,
        /// JSON-RPC error data, when the server sent some.
        data: Option<Box<Value>>,
    },

    /// Too many requests for this API key (HTTP 429), after the client's own retries.
    #[error("{message}")]
    RateLimit {
        /// HTTP status code.
        status: u16,
        /// JSON-RPC error code, when the server sent one.
        code: Option<i64>,
        /// The server's message.
        message: String,
        /// JSON-RPC error data, when the server sent some.
        data: Option<Box<Value>>,
        /// How long the server asked to wait (`Retry-After`), when it said.
        retry_after: Option<Duration>,
    },

    /// RapidWorkers is temporarily unavailable (HTTP 502, 503 or 504).
    #[error("{message}")]
    ServiceUnavailable {
        /// HTTP status code.
        status: u16,
        /// JSON-RPC error code, when the server sent one.
        code: Option<i64>,
        /// The server's message.
        message: String,
        /// JSON-RPC error data, when the server sent some.
        data: Option<Box<Value>>,
    },

    /// The server answered with another HTTP error, a JSON-RPC error or an unreadable body.
    #[error("{message}")]
    Api {
        /// HTTP status code.
        status: u16,
        /// JSON-RPC error code, when the server sent one.
        code: Option<i64>,
        /// The server's message.
        message: String,
        /// JSON-RPC error data, when the server sent some.
        data: Option<Box<Value>>,
    },

    /// The request did not reach RapidWorkers, or timed out with no answer.
    ///
    /// A call that moves money may still have run: retry it with the same `idempotency_key`.
    #[error("{message}")]
    Connection {
        /// What failed, including the underlying cause.
        message: String,
        /// The underlying transport error.
        #[source]
        source: Box<dyn std::error::Error + Send + Sync + 'static>,
    },
}

impl Error {
    /// The HTTP status code of an HTTP or JSON-RPC error.
    pub fn status(&self) -> Option<u16> {
        match self {
            Self::Authentication { status, .. }
            | Self::RateLimit { status, .. }
            | Self::ServiceUnavailable { status, .. }
            | Self::Api { status, .. } => Some(*status),
            _ => None,
        }
    }

    /// The machine-readable code of a [`Error::Tool`] (its `details.code`), such as
    /// `"insufficient_balance"`.
    pub fn tool_code(&self) -> Option<&str> {
        match self {
            Self::Tool { details, .. } => details.get("code").and_then(Value::as_str),
            _ => None,
        }
    }

    /// Whether this is a [`Error::Connection`] caused by a timeout.
    pub fn is_timeout(&self) -> bool {
        match self {
            Self::Connection { source, .. } => source
                .downcast_ref::<reqwest::Error>()
                .is_some_and(reqwest::Error::is_timeout),
            _ => false,
        }
    }

    pub(crate) fn connection(error: reqwest::Error) -> Self {
        let mut message = format!("Could not reach RapidWorkers: {error}");
        let mut cause = std::error::Error::source(&error);
        while let Some(inner) = cause {
            let text = inner.to_string();
            if !message.ends_with(&text) {
                message.push_str(": ");
                message.push_str(&text);
            }
            cause = inner.source();
        }
        Self::Connection {
            message,
            source: Box::new(error),
        }
    }
}
