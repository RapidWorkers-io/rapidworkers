//! The HTTP client for the hosted RapidWorkers MCP server.
//!
//! Each tool call is one JSON-RPC `tools/call` POST to the stateless MCP endpoint, so the SDK
//! gets exactly the validation, cost quotes, safeguards and error messages the server gives AI
//! agents.

use std::fmt;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderName, HeaderValue, ACCEPT, CONTENT_TYPE, RETRY_AFTER};
use reqwest::Url;
use serde::Serialize;
use serde_json::{Map, Value};
use uuid::Uuid;

use crate::error::{Error, Result};
use crate::generated::{tool_meta, SERVER_URL};

/// Environment variable holding the API key.
pub const API_KEY_ENV: &str = "RAPIDWORKERS_API_KEY";
/// Environment variable that overrides the MCP endpoint.
pub const BASE_URL_ENV: &str = "RAPIDWORKERS_MCP_URL";
/// Where to create an API key.
pub const API_KEYS_URL: &str = "https://www.rapidworkers.io/dashboard/api-management/";
/// The MCP protocol version sent with every request.
pub const PROTOCOL_VERSION: &str = "2025-06-18";
/// Default time limit for one HTTP request.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(60);
/// Default number of retries after a retryable failure.
pub const DEFAULT_MAX_RETRIES: u32 = 2;
/// This crate's version, sent in the `User-Agent` header.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");

const IDEMPOTENCY_KEY: &str = "idempotency_key";
const MAX_BACKOFF_SECS: f64 = 30.0;

/// Answered before any tool ran: safe to resend for every call.
fn retry_always(status: u16) -> bool {
    matches!(status, 429 | 503)
}

/// A gateway error may come after the tool ran: resend only retry-safe calls.
fn retry_if_safe(status: u16) -> bool {
    matches!(status, 502 | 504)
}

/// Asynchronous client for RapidWorkers.
///
/// Every tool of the RapidWorkers MCP server is a method. Calls that move money get an
/// idempotency key automatically, and a timed-out call is retried with the same key, so it never
/// happens twice.
///
/// ```no_run
/// # async fn run() -> rapidworkers::Result<()> {
/// let rw = rapidworkers::Client::from_env()?; // reads RAPIDWORKERS_API_KEY
/// let account = rw.get_account().await?;
/// println!("{}", account["balance"]);
/// # Ok(())
/// # }
/// ```
///
/// The client is cheap to clone: clones share one connection pool.
#[derive(Clone)]
pub struct Client {
    inner: Arc<Inner>,
}

struct Inner {
    http: reqwest::Client,
    base_url: Url,
    timeout: Duration,
    max_retries: u32,
    next_id: AtomicU64,
}

impl fmt::Debug for Client {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Client")
            .field("base_url", &self.inner.base_url.as_str())
            .field("timeout", &self.inner.timeout)
            .field("max_retries", &self.inner.max_retries)
            .finish_non_exhaustive()
    }
}

/// Configures a [`Client`]. Create one with [`Client::builder`].
#[derive(Clone, Default)]
pub struct ClientBuilder {
    api_key: Option<String>,
    base_url: Option<String>,
    timeout: Option<Duration>,
    max_retries: Option<u32>,
}

impl fmt::Debug for ClientBuilder {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ClientBuilder")
            .field("api_key", &self.api_key.as_ref().map(|_| "<redacted>"))
            .field("base_url", &self.base_url)
            .field("timeout", &self.timeout)
            .field("max_retries", &self.max_retries)
            .finish()
    }
}

impl ClientBuilder {
    /// The API key. Defaults to the `RAPIDWORKERS_API_KEY` environment variable.
    #[must_use]
    pub fn api_key(mut self, api_key: impl Into<String>) -> Self {
        self.api_key = Some(api_key.into());
        self
    }

    /// The MCP endpoint. Defaults to `RAPIDWORKERS_MCP_URL`, else [`SERVER_URL`].
    #[must_use]
    pub fn base_url(mut self, base_url: impl Into<String>) -> Self {
        self.base_url = Some(base_url.into());
        self
    }

    /// Time limit for one HTTP request (each retry gets its own). Default 60 seconds.
    #[must_use]
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = Some(timeout);
        self
    }

    /// How many times a retryable failure is retried. Default 2.
    ///
    /// HTTP 429 and 503 answers are retried for every call. Network errors and HTTP 502/504 are
    /// retried only for calls that are safe to resend: read-only or idempotent tools, and tools
    /// that take an idempotency key (the same key is sent again).
    #[must_use]
    pub fn max_retries(mut self, max_retries: u32) -> Self {
        self.max_retries = Some(max_retries);
        self
    }

    /// Builds the client.
    ///
    /// Fails with [`Error::Configuration`] when there is no API key or the endpoint is not a
    /// valid URL.
    pub fn build(self) -> Result<Client> {
        let api_key = resolve_api_key(self.api_key, || std::env::var(API_KEY_ENV).ok())?;
        let base_url = self
            .base_url
            .filter(|url| !url.is_empty())
            .or_else(|| {
                std::env::var(BASE_URL_ENV)
                    .ok()
                    .map(|url| url.trim().to_owned())
                    .filter(|url| !url.is_empty())
            })
            .unwrap_or_else(|| SERVER_URL.to_owned());
        let base_url = Url::parse(&base_url).map_err(|err| {
            Error::Configuration(format!("Invalid RapidWorkers endpoint {base_url:?}: {err}"))
        })?;
        let timeout = self.timeout.unwrap_or(DEFAULT_TIMEOUT);
        let http = reqwest::Client::builder()
            .default_headers(headers(&api_key)?)
            .user_agent(format!("rapidworkers-rust/{VERSION}"))
            .timeout(timeout)
            .build()
            .map_err(|err| {
                Error::Configuration(format!("Could not create the HTTP client: {err}"))
            })?;
        Ok(Client {
            inner: Arc::new(Inner {
                http,
                base_url,
                timeout,
                max_retries: self.max_retries.unwrap_or(DEFAULT_MAX_RETRIES),
                next_id: AtomicU64::new(1),
            }),
        })
    }
}

/// The explicit key, else the environment's (read only when needed), trimmed.
fn resolve_api_key(
    explicit: Option<String>,
    env: impl FnOnce() -> Option<String>,
) -> Result<String> {
    let key = explicit
        .filter(|key| !key.is_empty())
        .or_else(env)
        .unwrap_or_default();
    let key = key.trim();
    if key.is_empty() {
        return Err(Error::Configuration(format!(
            "No RapidWorkers API key. Pass one to Client::new or set {API_KEY_ENV}. \
             Create a key at {API_KEYS_URL}"
        )));
    }
    Ok(key.to_owned())
}

fn headers(api_key: &str) -> Result<HeaderMap> {
    let mut key = HeaderValue::from_str(api_key).map_err(|_| {
        Error::Configuration(
            "The API key contains characters that cannot be sent in an HTTP header.".into(),
        )
    })?;
    key.set_sensitive(true);
    let mut headers = HeaderMap::new();
    headers.insert(HeaderName::from_static("x-api-key"), key);
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    headers.insert(
        ACCEPT,
        HeaderValue::from_static("application/json, text/event-stream"),
    );
    headers.insert(
        HeaderName::from_static("mcp-protocol-version"),
        HeaderValue::from_static(PROTOCOL_VERSION),
    );
    Ok(headers)
}

impl Client {
    /// A client with this API key. The endpoint is `RAPIDWORKERS_MCP_URL` when set, else
    /// [`SERVER_URL`]. An empty key falls back to `RAPIDWORKERS_API_KEY`.
    pub fn new(api_key: impl Into<String>) -> Result<Self> {
        Self::builder().api_key(api_key).build()
    }

    /// A client configured from `RAPIDWORKERS_API_KEY` and, optionally, `RAPIDWORKERS_MCP_URL`.
    pub fn from_env() -> Result<Self> {
        Self::builder().build()
    }

    /// A builder to set the API key, endpoint, timeout and retries.
    pub fn builder() -> ClientBuilder {
        ClientBuilder::default()
    }

    /// The MCP endpoint this client posts to.
    pub fn base_url(&self) -> &str {
        self.inner.base_url.as_str()
    }

    /// The time limit for one HTTP request.
    pub fn timeout(&self) -> Duration {
        self.inner.timeout
    }

    /// How many times a retryable failure is retried.
    pub fn max_retries(&self) -> u32 {
        self.inner.max_retries
    }

    /// Calls any tool by name and returns its structured result.
    ///
    /// `null` arguments are dropped, and tools that take an `idempotency_key` get one when it
    /// is missing. Fails with [`Error::Tool`] when the tool refuses the call.
    pub async fn call_tool(&self, name: &str, arguments: Value) -> Result<Value> {
        let arguments = prepare(name, arguments)?;
        let mut params = Map::new();
        params.insert("name".into(), Value::String(name.to_owned()));
        params.insert("arguments".into(), arguments);
        self.request("tools/call", Value::Object(params)).await
    }

    /// The live tool definitions (`name`, `description`, `inputSchema`, `annotations`).
    pub async fn list_tools(&self) -> Result<Vec<Value>> {
        let result = self.request("tools/list", Value::Null).await?;
        match result.get("tools") {
            Some(Value::Array(tools)) => Ok(tools.clone()),
            _ => Ok(Vec::new()),
        }
    }

    /// Sends one JSON-RPC request (for example `tools/list`) and returns its `result`.
    ///
    /// For `tools/call` the result is unwrapped like [`Client::call_tool`] does, but the
    /// arguments are sent as given.
    pub async fn request(&self, method: &str, params: Value) -> Result<Value> {
        let params = match params {
            Value::Null => Value::Object(Map::new()),
            other => other,
        };
        let retry_safe = can_retry(method, &params);
        let mut attempt: u32 = 0;
        loop {
            let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed);
            let mut payload = Map::new();
            payload.insert("jsonrpc".into(), Value::from("2.0"));
            payload.insert("id".into(), Value::from(id));
            payload.insert("method".into(), Value::from(method));
            payload.insert("params".into(), params.clone());
            let body = Value::Object(payload).to_string();
            match self.post(body).await {
                Err(err) => {
                    if retry_safe && attempt < self.inner.max_retries {
                        tokio::time::sleep(backoff(attempt, None)).await;
                        attempt += 1;
                        continue;
                    }
                    return Err(Error::connection(err));
                }
                Ok(answer) => {
                    if should_retry(answer.status, retry_safe) && attempt < self.inner.max_retries {
                        tokio::time::sleep(backoff(attempt, answer.retry_after)).await;
                        attempt += 1;
                        continue;
                    }
                    return unwrap(method, &params, &answer, id);
                }
            }
        }
    }

    async fn post(&self, body: String) -> std::result::Result<HttpAnswer, reqwest::Error> {
        let response = self
            .inner
            .http
            .post(self.inner.base_url.clone())
            .body(body)
            .send()
            .await?;
        let status = response.status().as_u16();
        let header = |name| {
            response
                .headers()
                .get(name)
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned)
        };
        let content_type = header(CONTENT_TYPE).unwrap_or_default();
        let retry_after = header(RETRY_AFTER).and_then(|value| parse_retry_after(&value));
        let body = response.bytes().await?.to_vec();
        Ok(HttpAnswer {
            status,
            content_type,
            retry_after,
            body,
        })
    }
}

/// Serializes typed tool arguments.
pub(crate) fn to_arguments<T: Serialize>(params: &T) -> Result<Value> {
    serde_json::to_value(params)
        .map_err(|err| Error::Configuration(format!("Could not encode the tool arguments: {err}")))
}

struct HttpAnswer {
    status: u16,
    content_type: String,
    retry_after: Option<f64>,
    body: Vec<u8>,
}

impl HttpAnswer {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }
}

/// Drops `null` arguments and adds an idempotency key where the tool takes one.
fn prepare(name: &str, arguments: Value) -> Result<Value> {
    let mut arguments = match arguments {
        Value::Null => Map::new(),
        Value::Object(map) => map,
        other => {
            return Err(Error::Configuration(format!(
                "Tool arguments must be a JSON object, not {other}"
            )))
        }
    };
    arguments.retain(|_, value| !value.is_null());
    let takes_key = tool_meta(name).is_some_and(|meta| meta.idempotency_key);
    if takes_key && !arguments.contains_key(IDEMPOTENCY_KEY) {
        let key = format!("sdk-{}", Uuid::new_v4().simple());
        arguments.insert(IDEMPOTENCY_KEY.into(), Value::String(key));
    }
    Ok(Value::Object(arguments))
}

fn can_retry(method: &str, params: &Value) -> bool {
    if method != "tools/call" {
        return true;
    }
    params
        .get("name")
        .and_then(Value::as_str)
        .and_then(tool_meta)
        .is_some_and(|meta| meta.retry)
}

fn should_retry(status: u16, retry_safe: bool) -> bool {
    retry_always(status) || (retry_safe && retry_if_safe(status))
}

/// `Retry-After` in seconds; HTTP dates are ignored.
fn parse_retry_after(value: &str) -> Option<f64> {
    let seconds: f64 = value.trim().parse().ok()?;
    Some(if seconds.is_nan() {
        0.0
    } else {
        seconds.max(0.0)
    })
}

/// 0.5 s, 1 s, 2 s... capped at 30 s, or what `Retry-After` asked for (also capped).
fn backoff(attempt: u32, retry_after: Option<f64>) -> Duration {
    let exponent = i32::try_from(attempt).unwrap_or(i32::MAX);
    let seconds = retry_after.unwrap_or_else(|| 0.5 * 2f64.powi(exponent));
    Duration::from_secs_f64(seconds.min(MAX_BACKOFF_SECS))
}

fn id_matches(value: Option<&Value>, id: u64) -> bool {
    match value {
        Some(Value::Number(number)) => {
            number.as_u64() == Some(id) || number.as_f64().is_some_and(|n| n == id as f64)
        }
        _ => false,
    }
}

/// The JSON-RPC answer to request `id` from a JSON or SSE response body.
fn rpc_message(answer: &HttpAnswer, id: u64) -> Option<Map<String, Value>> {
    if answer.content_type.contains("text/event-stream") {
        let text = answer.text();
        return text.split(['\r', '\n']).find_map(|line| {
            let data = line.strip_prefix("data:")?;
            match serde_json::from_str(data.trim()) {
                Ok(Value::Object(message)) if id_matches(message.get("id"), id) => Some(message),
                _ => None,
            }
        });
    }
    match serde_json::from_slice(&answer.body) {
        Ok(Value::Object(message)) => Some(message),
        Ok(Value::Array(messages)) => messages.into_iter().find_map(|message| match message {
            Value::Object(message) if id_matches(message.get("id"), id) => Some(message),
            _ => None,
        }),
        _ => None,
    }
}

/// The non-empty string at `key`.
fn text_field<'a>(object: Option<&'a Map<String, Value>>, key: &str) -> Option<&'a str> {
    object
        .and_then(|object| object.get(key))
        .and_then(Value::as_str)
        .filter(|text| !text.is_empty())
}

fn error_parts(error: Option<&Map<String, Value>>) -> (Option<i64>, Option<Box<Value>>) {
    let code = error
        .and_then(|error| error.get("code"))
        .and_then(Value::as_i64);
    let data = error
        .and_then(|error| error.get("data"))
        .filter(|data| !data.is_null())
        .map(|data| Box::new(data.clone()));
    (code, data)
}

fn http_error(answer: &HttpAnswer, message: Option<&Map<String, Value>>) -> Error {
    let error = message
        .and_then(|message| message.get("error"))
        .and_then(Value::as_object);
    let text = match text_field(error, "message") {
        Some(text) => text.to_owned(),
        None => {
            let body: String = answer.text().chars().take(500).collect();
            if body.is_empty() {
                format!("HTTP {}", answer.status)
            } else {
                body
            }
        }
    };
    let (code, data) = error_parts(error);
    let status = answer.status;
    match status {
        401 => Error::Authentication {
            status,
            code,
            message: format!("{text} Check your API key or create one at {API_KEYS_URL}"),
            data,
        },
        429 => Error::RateLimit {
            status,
            code,
            message: text,
            data,
            retry_after: answer
                .retry_after
                .map(|seconds| Duration::try_from_secs_f64(seconds).unwrap_or(Duration::MAX)),
        },
        502..=504 => Error::ServiceUnavailable {
            status,
            code,
            message: text,
            data,
        },
        _ => Error::Api {
            status,
            code,
            message: text,
            data,
        },
    }
}

fn unwrap(method: &str, params: &Value, answer: &HttpAnswer, id: u64) -> Result<Value> {
    let message = rpc_message(answer, id);
    if answer.status >= 400 {
        return Err(http_error(answer, message.as_ref()));
    }
    let Some(mut message) = message else {
        return Err(Error::Api {
            status: answer.status,
            code: None,
            message: "Unreadable answer from RapidWorkers.".into(),
            data: None,
        });
    };
    if let Some(error) = message.get("error") {
        let error = error.as_object();
        let (code, data) = error_parts(error);
        return Err(Error::Api {
            status: answer.status,
            code,
            message: text_field(error, "message")
                .unwrap_or("JSON-RPC error")
                .to_owned(),
            data,
        });
    }
    let result = message.remove("result").unwrap_or(Value::Null);
    if method != "tools/call" {
        return Ok(result);
    }
    let tool = match params.get("name") {
        Some(Value::String(name)) => name.clone(),
        Some(other) => other.to_string(),
        None => String::new(),
    };
    tool_result(tool, result)
}

/// Python-style truthiness, for flags such as `isError`.
fn truthy(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(number)) => number.as_f64() != Some(0.0),
        Some(Value::String(text)) => !text.is_empty(),
        Some(Value::Array(items)) => !items.is_empty(),
        Some(Value::Object(map)) => !map.is_empty(),
    }
}

fn tool_result(tool: String, result: Value) -> Result<Value> {
    let mut result = match result {
        Value::Object(result) => result,
        _ => Map::new(),
    };
    let structured = result.remove("structuredContent");
    let text = match result.get("content") {
        Some(Value::Array(blocks)) => blocks
            .iter()
            .filter(|block| block.get("type").and_then(Value::as_str) == Some("text"))
            .map(|block| block.get("text").and_then(Value::as_str).unwrap_or(""))
            .collect::<Vec<_>>()
            .join("\n"),
        _ => String::new(),
    };
    if truthy(result.get("isError")) {
        let mut details = match structured {
            Some(Value::Object(mut structured)) => match structured.remove("error") {
                Some(Value::Object(error)) => error,
                _ => Map::new(),
            },
            _ => Map::new(),
        };
        let message = match details.remove("message") {
            Some(Value::String(message)) if !message.is_empty() => message,
            _ if !text.is_empty() => text,
            _ => "The tool call failed.".into(),
        };
        return Err(Error::Tool {
            tool,
            message,
            details: Box::new(details),
        });
    }
    if let Some(Value::Object(structured)) = structured {
        return Ok(Value::Object(structured));
    }
    // Text-only answers carry a one-line summary, a blank line and the JSON
    let payload = match text.split_once("\n\n") {
        Some((_, payload)) if !payload.is_empty() => payload,
        _ => text.as_str(),
    };
    let data = match serde_json::from_str(payload) {
        Ok(Value::Object(data)) => Value::Object(data),
        Ok(other) => single("data", other),
        Err(_) => single("text", Value::String(text)),
    };
    Ok(data)
}

fn single(key: &str, value: Value) -> Value {
    let mut map = Map::new();
    map.insert(key.into(), value);
    Value::Object(map)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn backoff_doubles_and_is_capped() {
        assert_eq!(backoff(0, None), Duration::from_millis(500));
        assert_eq!(backoff(1, None), Duration::from_secs(1));
        assert_eq!(backoff(2, None), Duration::from_secs(2));
        assert_eq!(backoff(10, None), Duration::from_secs(30));
        assert_eq!(backoff(u32::MAX, None), Duration::from_secs(30));
        assert_eq!(backoff(0, Some(3.0)), Duration::from_secs(3));
        assert_eq!(backoff(0, Some(120.0)), Duration::from_secs(30));
        assert_eq!(backoff(0, Some(f64::INFINITY)), Duration::from_secs(30));
    }

    #[test]
    fn retry_after_is_parsed_like_python() {
        assert_eq!(parse_retry_after("2"), Some(2.0));
        assert_eq!(parse_retry_after(" 1.5 "), Some(1.5));
        assert_eq!(parse_retry_after("-4"), Some(0.0));
        assert_eq!(parse_retry_after("NaN"), Some(0.0));
        assert_eq!(parse_retry_after(""), None);
        assert_eq!(parse_retry_after("Wed, 21 Oct 2026 07:28:00 GMT"), None);
    }

    #[test]
    fn retry_policy() {
        let safe = json!({"name": "approve_submission", "arguments": {}});
        let unsafe_write = json!({"name": "get_deposit_link", "arguments": {}});
        let unknown = json!({"name": "no_such_tool", "arguments": {}});
        assert!(can_retry("tools/call", &safe));
        assert!(!can_retry("tools/call", &unsafe_write));
        assert!(!can_retry("tools/call", &unknown));
        assert!(can_retry("tools/list", &Value::Null));
        for status in [429, 503] {
            assert!(should_retry(status, false));
            assert!(should_retry(status, true));
        }
        for status in [502, 504] {
            assert!(!should_retry(status, false));
            assert!(should_retry(status, true));
        }
        for status in [200, 400, 401, 500] {
            assert!(!should_retry(status, true));
        }
    }

    #[test]
    fn api_key_resolution() {
        let env = || Some("from-env".to_owned());
        let unread = || -> Option<String> { panic!("the environment is read only when needed") };
        assert_eq!(resolve_api_key(Some(" k ".into()), unread).unwrap(), "k");
        assert_eq!(
            resolve_api_key(Some(String::new()), env).unwrap(),
            "from-env"
        );
        assert_eq!(resolve_api_key(None, env).unwrap(), "from-env");
        let missing = resolve_api_key(None, || None).unwrap_err();
        assert!(matches!(missing, Error::Configuration(_)));
        assert!(missing.to_string().contains(API_KEYS_URL));
        assert!(resolve_api_key(Some("   ".into()), unread).is_err());
    }

    #[test]
    fn prepare_drops_nulls_and_adds_a_key_only_where_taken() {
        let args = prepare("list_jobs", json!({"kind": "job", "status": null})).unwrap();
        assert_eq!(args, json!({"kind": "job"}));
        let args = prepare("cancel_job", Value::Null).unwrap();
        let key = args[IDEMPOTENCY_KEY].as_str().unwrap();
        assert!(key.starts_with("sdk-") && key.len() == 36);
        let args = prepare("cancel_job", json!({"idempotency_key": null})).unwrap();
        assert!(args[IDEMPOTENCY_KEY].as_str().unwrap().starts_with("sdk-"));
        assert!(prepare("get_account", json!([1])).is_err());
    }

    #[test]
    fn text_results_fall_back_like_python() {
        let result = |text: &str| json!({"content": [{"type": "text", "text": text}]});
        let tool = || "t".to_owned();
        assert_eq!(
            tool_result(tool(), result("One job.\n\n{\"jobs\": [1]}")).unwrap(),
            json!({"jobs": [1]})
        );
        assert_eq!(
            tool_result(tool(), result("{\"a\": 1}")).unwrap(),
            json!({"a": 1})
        );
        assert_eq!(
            tool_result(tool(), result("x\n\n[1, 2]")).unwrap(),
            json!({"data": [1, 2]})
        );
        assert_eq!(
            tool_result(tool(), result("plain")).unwrap(),
            json!({"text": "plain"})
        );
        assert_eq!(
            tool_result(tool(), Value::Null).unwrap(),
            json!({"text": ""})
        );
    }

    #[test]
    fn tool_errors_without_structured_content_use_the_text() {
        let result = json!({"content": [{"type": "text", "text": "Nope."}], "isError": true});
        match tool_result("get_job".into(), result).unwrap_err() {
            Error::Tool {
                tool,
                message,
                details,
            } => {
                assert_eq!(tool, "get_job");
                assert_eq!(message, "Nope.");
                assert!(details.is_empty());
            }
            other => panic!("unexpected {other:?}"),
        }
        let empty = tool_result("x".into(), json!({"isError": true})).unwrap_err();
        assert_eq!(empty.to_string(), "The tool call failed.");
    }
}
