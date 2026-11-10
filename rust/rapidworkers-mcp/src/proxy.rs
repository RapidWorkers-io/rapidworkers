//! Answers one JSON-RPC message: locally (ping, or everything without an API key) or by
//! relaying it unchanged to the hosted RapidWorkers MCP server.

use std::sync::Mutex;
use std::time::Duration;

use reqwest::header::{HeaderValue, ACCEPT, CONTENT_TYPE, RETRY_AFTER};
use serde_json::{json, Map, Value};

pub const VERSION: &str = env!("CARGO_PKG_VERSION");
pub const API_KEY_ENV: &str = "RAPIDWORKERS_API_KEY";
pub const URL_ENV: &str = "RAPIDWORKERS_MCP_URL";
pub const API_KEYS_URL: &str = "https://www.rapidworkers.io/dashboard/api-management/";
const KNOWN_PROTOCOL_VERSIONS: [&str; 4] = ["2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"];
const DEFAULT_PROTOCOL_VERSION: &str = "2025-06-18";
/// Answered before anything ran upstream, so resending is always safe.
const RETRY_STATUS: [u16; 2] = [429, 503];
const MAX_RETRIES: u32 = 2;

pub const PARSE_ERROR: i64 = -32700;
pub const INVALID_REQUEST: i64 = -32600;
pub const METHOD_NOT_FOUND: i64 = -32601;
pub const INTERNAL_ERROR: i64 = -32603;
pub const UPSTREAM_ERROR: i64 = -32000;

/// The tools/list snapshot bundled at build time (scripts/codegen.py writes it from spec/tools.json).
pub const BUNDLED_SPEC: &str = include_str!("tools.json");

pub fn no_key_message() -> String {
    format!(
        "{API_KEY_ENV} is not set. Create an API key at {API_KEYS_URL} and add it to this MCP \
         server's environment (for example \"env\": {{\"{API_KEY_ENV}\": \"...\"}}), then restart \
         the client."
    )
}

pub fn log(message: &str) {
    eprintln!("[rapidworkers-mcp] {message}");
}

pub fn error_response(id: Value, code: Value, message: &str, data: Option<Value>) -> Value {
    let mut error = Map::new();
    error.insert("code".into(), code);
    error.insert("message".into(), Value::from(message));
    if let Some(data) = data {
        error.insert("data".into(), data);
    }
    json!({"jsonrpc": "2.0", "id": id, "error": error})
}

pub fn result_response(id: Value, result: Value) -> Value {
    json!({"jsonrpc": "2.0", "id": id, "result": result})
}

/// Python-style truthiness, used where the Python server writes `x or default`.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64() != Some(0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// JSON-RPC ids compare like Python values: `1 == 1.0`.
fn same_id(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(x), Value::Number(y)) => match (x.as_i64(), y.as_i64()) {
            (Some(x), Some(y)) => x == y,
            _ => x.as_f64() == y.as_f64(),
        },
        _ => a == b,
    }
}

/// Seconds to wait before retry number `attempt` (1-based): `Retry-After` when it is a number,
/// else 0.5 s doubled per attempt; at most 30 s.
pub fn retry_delay(retry_after: Option<&str>, attempt: u32) -> Duration {
    let hinted = retry_after.and_then(|value| value.trim().parse::<f64>().ok());
    let seconds = match hinted {
        Some(seconds) if seconds.is_nan() => 0.0,
        Some(seconds) => seconds.max(0.0),
        None => 0.5 * 2f64.powi(i32::try_from(attempt).unwrap_or(i32::MAX)),
    };
    Duration::from_secs_f64(seconds.min(30.0))
}

pub struct Upstream {
    pub status: u16,
    pub content_type: String,
    pub body: Vec<u8>,
}

/// The JSON-RPC message answering `request_id`, from a JSON or SSE body.
pub fn parse_upstream(upstream: &Upstream, request_id: &Value) -> Option<Map<String, Value>> {
    let matches = |message: &Map<String, Value>| {
        same_id(message.get("id").unwrap_or(&Value::Null), request_id)
    };
    if upstream.content_type.contains("text/event-stream") {
        let text = String::from_utf8_lossy(&upstream.body);
        return text.split(['\r', '\n']).find_map(|line| {
            let data = line.strip_prefix("data:")?;
            match serde_json::from_str(data.trim()) {
                Ok(Value::Object(message)) if matches(&message) => Some(message),
                _ => None,
            }
        });
    }
    match serde_json::from_slice(&upstream.body) {
        Ok(Value::Object(message)) => Some(message),
        Ok(Value::Array(messages)) => messages.into_iter().find_map(|message| match message {
            Value::Object(message) if matches(&message) => Some(message),
            _ => None,
        }),
        _ => None,
    }
}

/// A short name for a transport failure, like the exception class the Python server reports.
fn failure_kind(error: &reqwest::Error) -> &'static str {
    if error.is_timeout() {
        "Timeout"
    } else if error.is_connect() {
        "ConnectError"
    } else if error.is_body() || error.is_decode() {
        "ReadError"
    } else {
        "RequestError"
    }
}

fn describe(error: &reqwest::Error) -> String {
    let mut text = error.to_string();
    let mut cause = std::error::Error::source(error);
    while let Some(inner) = cause {
        text.push_str(": ");
        text.push_str(&inner.to_string());
        cause = inner.source();
    }
    text
}

pub struct Proxy {
    api_key: String,
    url: String,
    spec: Value,
    protocol_version: Mutex<Option<String>>,
    http: reqwest::Client,
}

impl Proxy {
    pub fn new(api_key: String, url: String, spec: Value) -> Result<Self, reqwest::Error> {
        let http = reqwest::Client::builder()
            .user_agent(format!("rapidworkers-mcp-rust/{VERSION}"))
            .connect_timeout(Duration::from_secs(15))
            .read_timeout(Duration::from_secs(120))
            .build()?;
        Ok(Self {
            api_key,
            url,
            spec,
            protocol_version: Mutex::new(None),
            http,
        })
    }

    fn protocol_version(&self) -> Option<String> {
        self.protocol_version
            .lock()
            .map(|version| version.clone())
            .unwrap_or_default()
    }

    fn set_protocol_version(&self, version: Option<String>) {
        if let Ok(mut current) = self.protocol_version.lock() {
            *current = version;
        }
    }

    /// The answer to one client message, or `None` for notifications and responses.
    pub async fn handle(&self, message: Value) -> Option<Value> {
        let Value::Object(map) = &message else {
            return Some(invalid_request());
        };
        if map.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
            return Some(invalid_request());
        }
        // A response to a server request; this server sends none
        let method = map.get("method").and_then(Value::as_str)?;
        // Notifications (initialized, cancelled...) need no answer
        let request_id = map.get("id")?.clone();
        if method == "ping" {
            return Some(result_response(request_id, json!({})));
        }
        if self.api_key.is_empty() {
            return Some(self.offline(request_id, method, map.get("params")));
        }
        Some(self.forward(&message, request_id, method).await)
    }

    async fn forward(&self, message: &Value, request_id: Value, method: &str) -> Value {
        let body = message.to_string();
        let mut attempt = 0;
        let upstream = loop {
            let mut request = self
                .http
                .post(&self.url)
                .header("X-API-Key", &self.api_key)
                .header(CONTENT_TYPE, "application/json")
                .header(ACCEPT, "application/json, text/event-stream")
                .body(body.clone());
            if let Some(version) = self.protocol_version() {
                request = request.header("MCP-Protocol-Version", version);
            }
            let response = match request.send().await {
                Ok(response) => response,
                Err(error) => return self.unreachable(request_id, method, &error),
            };
            let status = response.status().as_u16();
            let header = |name| {
                response
                    .headers()
                    .get(name)
                    .and_then(|value: &HeaderValue| value.to_str().ok())
                    .map(str::to_owned)
            };
            if RETRY_STATUS.contains(&status) && attempt < MAX_RETRIES {
                attempt += 1;
                let delay = retry_delay(header(RETRY_AFTER).as_deref(), attempt);
                tokio::time::sleep(delay).await;
                continue;
            }
            let content_type = header(CONTENT_TYPE).unwrap_or_default();
            match response.bytes().await {
                Ok(body) => {
                    break Upstream {
                        status,
                        content_type,
                        body: body.to_vec(),
                    }
                }
                Err(error) => return self.unreachable(request_id, method, &error),
            }
        };

        let mut answer = match parse_upstream(&upstream, &request_id) {
            Some(answer) if upstream.status < 400 => answer,
            parsed => return upstream_error(request_id, upstream.status, parsed.as_ref()),
        };
        if method == "initialize" {
            if let Some(Value::Object(result)) = answer.get("result") {
                let version = result.get("protocolVersion").and_then(Value::as_str);
                self.set_protocol_version(version.map(str::to_owned));
            }
        }
        answer.insert("id".into(), request_id);
        Value::Object(answer)
    }

    fn unreachable(&self, request_id: Value, method: &str, error: &reqwest::Error) -> Value {
        log(&format!(
            "request to {} failed: {}",
            self.url,
            describe(error)
        ));
        let note = if method == "tools/call" {
            " If this call moves money, retry it with the same idempotency_key."
        } else {
            ""
        };
        let text = format!(
            "Could not reach RapidWorkers ({}).{note}",
            failure_kind(error)
        );
        error_response(request_id, Value::from(UPSTREAM_ERROR), &text, None)
    }

    /// Answers without an API key: enough to start and list tools.
    pub fn offline(&self, request_id: Value, method: &str, params: Option<&Value>) -> Value {
        let server = &self.spec["server"];
        match method {
            "initialize" => {
                let requested = params
                    .and_then(|params| params.get("protocolVersion"))
                    .and_then(Value::as_str);
                let version = requested
                    .filter(|version| KNOWN_PROTOCOL_VERSIONS.contains(version))
                    .unwrap_or(DEFAULT_PROTOCOL_VERSION);
                let instructions = format!(
                    "{}\n\n{}",
                    server["instructions"].as_str().unwrap_or_default(),
                    no_key_message()
                );
                let result = json!({
                    "protocolVersion": version,
                    "capabilities": {"tools": {"listChanged": false}},
                    "serverInfo": {
                        "name": server["name"],
                        "title": server["title"],
                        "version": VERSION,
                    },
                    "instructions": instructions,
                });
                result_response(request_id, result)
            }
            "tools/list" => result_response(request_id, json!({"tools": self.spec["tools"]})),
            "tools/call" => {
                let content = json!([{"type": "text", "text": no_key_message()}]);
                result_response(request_id, json!({"content": content, "isError": true}))
            }
            "resources/list" => result_response(request_id, json!({"resources": []})),
            "resources/templates/list" => {
                result_response(request_id, json!({"resourceTemplates": []}))
            }
            "prompts/list" => result_response(request_id, json!({"prompts": []})),
            _ => error_response(
                request_id,
                Value::from(METHOD_NOT_FOUND),
                &format!("{method} needs {API_KEY_ENV}. {}", no_key_message()),
                None,
            ),
        }
    }
}

/// A JSON-RPC error for an HTTP error or an unreadable upstream answer.
fn upstream_error(request_id: Value, status: u16, parsed: Option<&Map<String, Value>>) -> Value {
    let error = parsed
        .and_then(|message| message.get("error"))
        .and_then(Value::as_object);
    let field = |key| error.and_then(|error| error.get(key));
    let mut text = match field("message") {
        Some(Value::String(text)) if !text.is_empty() => text.clone(),
        _ => format!("RapidWorkers answered HTTP {status}."),
    };
    if status == 401 {
        text = format!("{text} Check {API_KEY_ENV} or create a new key at {API_KEYS_URL}");
    }
    let code = field("code")
        .filter(|code| truthy(code))
        .cloned()
        .unwrap_or(Value::from(UPSTREAM_ERROR));
    let data = field("data").filter(|data| !data.is_null()).cloned();
    error_response(request_id, code, &text, data)
}

fn invalid_request() -> Value {
    error_response(
        Value::Null,
        Value::from(INVALID_REQUEST),
        "Invalid JSON-RPC message.",
        None,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use wiremock::matchers::{header, method};
    use wiremock::{Mock, MockServer, Request, ResponseTemplate};

    const KEY: &str = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk";

    fn spec() -> Value {
        serde_json::from_str(BUNDLED_SPEC).unwrap()
    }

    fn rpc(method: &str, id: Value, params: Value) -> Value {
        json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params})
    }

    async fn proxy() -> (Proxy, MockServer) {
        let server = MockServer::start().await;
        let url = format!("{}/api/mcp/", server.uri());
        (Proxy::new(KEY.into(), url, spec()).unwrap(), server)
    }

    /// Answers with `result` and the id of the request.
    fn echo(result: Value) -> impl Fn(&Request) -> ResponseTemplate + Send + Sync {
        move |request: &Request| {
            let body: Value = serde_json::from_slice(&request.body).unwrap();
            ResponseTemplate::new(200).set_body_json(json!({
                "jsonrpc": "2.0",
                "id": body["id"],
                "result": result,
            }))
        }
    }

    async fn sent(server: &MockServer) -> Vec<Request> {
        server.received_requests().await.unwrap_or_default()
    }

    #[tokio::test]
    async fn forwards_request_unchanged_with_api_key() {
        let (proxy, server) = proxy().await;
        Mock::given(method("POST"))
            .respond_with(echo(json!({"ok": 1})))
            .mount(&server)
            .await;
        let params = json!({"name": "get_account", "arguments": {}, "_meta": {"progressToken": 3}});
        let message = rpc("tools/call", json!(7), params);
        let answer = proxy.handle(message.clone()).await.unwrap();
        assert_eq!(
            answer,
            json!({"jsonrpc": "2.0", "id": 7, "result": {"ok": 1}})
        );

        let requests = sent(&server).await;
        assert_eq!(requests.len(), 1);
        let request = &requests[0];
        assert_eq!(request.url.path(), "/api/mcp/");
        assert_eq!(String::from_utf8_lossy(&request.body), message.to_string());
        let header = |name: &str| request.headers.get(name).unwrap().to_str().unwrap();
        assert_eq!(header("x-api-key"), KEY);
        assert_eq!(header("content-type"), "application/json");
        assert_eq!(header("accept"), "application/json, text/event-stream");
        assert_eq!(
            header("user-agent"),
            format!("rapidworkers-mcp-rust/{VERSION}")
        );
        // No protocol version before initialize
        assert!(request.headers.get("mcp-protocol-version").is_none());
    }

    #[tokio::test]
    async fn key_order_is_kept_when_relaying() {
        let (proxy, server) = proxy().await;
        Mock::given(method("POST"))
            .respond_with(echo(json!({})))
            .mount(&server)
            .await;
        let line = r#"{"params":{"z":1,"a":2},"method":"tools/list","id":"x","jsonrpc":"2.0"}"#;
        let message: Value = serde_json::from_str(line).unwrap();
        proxy.handle(message).await.unwrap();
        let requests = sent(&server).await;
        assert_eq!(String::from_utf8_lossy(&requests[0].body), line);
    }

    #[tokio::test]
    async fn initialize_sets_protocol_header_for_later_calls() {
        let (proxy, server) = proxy().await;
        Mock::given(method("POST"))
            .respond_with(echo(json!({"protocolVersion": "2025-06-18"})))
            .mount(&server)
            .await;
        let init = rpc(
            "initialize",
            json!(1),
            json!({"protocolVersion": "2025-06-18"}),
        );
        proxy.handle(init).await.unwrap();
        proxy
            .handle(rpc("tools/list", json!(2), json!({})))
            .await
            .unwrap();
        let requests = sent(&server).await;
        assert!(requests[0].headers.get("mcp-protocol-version").is_none());
        let version = requests[1].headers.get("mcp-protocol-version").unwrap();
        assert_eq!(version, "2025-06-18");
    }

    #[tokio::test]
    async fn answer_id_is_the_client_id() {
        let (proxy, server) = proxy().await;
        let reply = json!({"jsonrpc": "2.0", "id": "other", "result": {"tools": []}});
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(reply))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/list", json!(11), json!({}))).await;
        assert_eq!(answer.unwrap()["id"], 11);
    }

    #[tokio::test]
    async fn sse_answer() {
        let (proxy, server) = proxy().await;
        Mock::given(method("POST"))
            .respond_with(|request: &Request| {
                let body: Value = serde_json::from_slice(&request.body).unwrap();
                let data = json!({"jsonrpc": "2.0", "id": body["id"], "result": {"tools": []}});
                let stream = format!("event: message\ndata: {data}\n\n");
                ResponseTemplate::new(200).set_body_raw(stream, "text/event-stream")
            })
            .mount(&server)
            .await;
        let answer = proxy
            .handle(rpc("tools/list", json!("abc"), json!({})))
            .await;
        let answer = answer.unwrap();
        assert_eq!(answer["result"], json!({"tools": []}));
        assert_eq!(answer["id"], "abc");
    }

    #[tokio::test]
    async fn status_401_becomes_jsonrpc_error_with_key_hint() {
        let (proxy, server) = proxy().await;
        let denied = json!({"jsonrpc": "2.0", "id": null, "error": {"code": -32001, "message": "Invalid token."}});
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(401).set_body_json(denied))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/list", json!(3), json!({}))).await;
        let answer = answer.unwrap();
        assert_eq!(answer["id"], 3);
        assert_eq!(answer["error"]["code"], -32001);
        let message = answer["error"]["message"].as_str().unwrap();
        assert!(message.starts_with("Invalid token. Check RAPIDWORKERS_API_KEY"));
        assert!(message.contains("api-management"));
        assert_eq!(sent(&server).await.len(), 1);
    }

    #[tokio::test]
    async fn http_error_without_jsonrpc_body() {
        let (proxy, server) = proxy().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(500).set_body_string("oops"))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/list", json!(4), json!({}))).await;
        let answer = answer.unwrap();
        assert_eq!(answer["error"]["code"], UPSTREAM_ERROR);
        assert_eq!(
            answer["error"]["message"],
            "RapidWorkers answered HTTP 500."
        );
        assert!(answer["error"].get("data").is_none());
    }

    #[tokio::test]
    async fn upstream_error_data_is_kept() {
        let (proxy, server) = proxy().await;
        let reply = json!({"jsonrpc": "2.0", "id": 5, "error": {"code": 0, "message": "", "data": {"x": 1}}});
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(400).set_body_json(reply))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/list", json!(5), json!({}))).await;
        let error = &answer.unwrap()["error"];
        assert_eq!(error["code"], UPSTREAM_ERROR);
        assert_eq!(error["message"], "RapidWorkers answered HTTP 400.");
        assert_eq!(error["data"], json!({"x": 1}));
    }

    #[tokio::test]
    async fn jsonrpc_error_with_status_200_is_relayed() {
        let (proxy, server) = proxy().await;
        let reply = json!({"jsonrpc": "2.0", "id": 6, "error": {"code": -32602, "message": "Unknown tool"}});
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(reply.clone()))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/call", json!(6), json!({}))).await;
        assert_eq!(answer.unwrap(), reply);
    }

    #[tokio::test]
    async fn status_503_is_retried() {
        let (proxy, server) = proxy().await;
        let unavailable = ResponseTemplate::new(503).insert_header("Retry-After", "0");
        Mock::given(method("POST"))
            .respond_with(unavailable)
            .up_to_n_times(1)
            .mount(&server)
            .await;
        let ok = json!({"jsonrpc": "2.0", "id": 4, "result": {}});
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(ok))
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/list", json!(4), json!({}))).await;
        assert_eq!(answer.unwrap()["result"], json!({}));
        assert_eq!(sent(&server).await.len(), 2);
    }

    #[tokio::test]
    async fn status_429_is_retried_twice_then_reported() {
        let (proxy, server) = proxy().await;
        let limited = ResponseTemplate::new(429).insert_header("Retry-After", "0");
        Mock::given(method("POST"))
            .and(header("x-api-key", KEY))
            .respond_with(limited)
            .mount(&server)
            .await;
        let answer = proxy.handle(rpc("tools/call", json!(8), json!({}))).await;
        let answer = answer.unwrap();
        assert_eq!(
            answer["error"]["message"],
            "RapidWorkers answered HTTP 429."
        );
        assert_eq!(sent(&server).await.len(), 3);
    }

    #[tokio::test]
    async fn network_error_on_tool_call_mentions_idempotency() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}/api/mcp/", listener.local_addr().unwrap());
        drop(listener);
        let proxy = Proxy::new(KEY.into(), url, spec()).unwrap();
        let params = json!({"name": "approve_submission", "arguments": {}});
        let answer = proxy
            .handle(rpc("tools/call", json!(5), params))
            .await
            .unwrap();
        assert_eq!(answer["id"], 5);
        assert_eq!(answer["error"]["code"], UPSTREAM_ERROR);
        let message = answer["error"]["message"].as_str().unwrap();
        assert!(message.starts_with("Could not reach RapidWorkers (ConnectError)."));
        assert!(message.contains("same idempotency_key"), "{message}");

        let answer = proxy
            .handle(rpc("tools/list", json!(6), json!({})))
            .await
            .unwrap();
        let message = answer["error"]["message"].as_str().unwrap();
        assert!(!message.contains("idempotency_key"), "{message}");
    }

    #[tokio::test]
    async fn notifications_responses_and_ping_are_handled_locally() {
        let (proxy, server) = proxy().await;
        let notification = json!({"jsonrpc": "2.0", "method": "notifications/initialized"});
        assert_eq!(proxy.handle(notification).await, None);
        let response = json!({"jsonrpc": "2.0", "id": 1, "result": {}});
        assert_eq!(proxy.handle(response).await, None);
        let ping = proxy.handle(rpc("ping", json!(9), json!({}))).await;
        assert_eq!(ping, Some(json!({"jsonrpc": "2.0", "id": 9, "result": {}})));
        assert!(sent(&server).await.is_empty());
    }

    #[tokio::test]
    async fn invalid_messages() {
        let (proxy, _server) = proxy().await;
        for message in [json!([1]), json!("x"), json!({"id": 1, "method": "ping"})] {
            let answer = proxy.handle(message).await.unwrap();
            assert_eq!(answer["error"]["code"], INVALID_REQUEST);
            assert_eq!(answer["id"], Value::Null);
        }
    }

    #[tokio::test]
    async fn without_key_answers_locally() {
        let proxy = Proxy::new(String::new(), "http://127.0.0.1:9/".into(), spec()).unwrap();
        let init = rpc(
            "initialize",
            json!(1),
            json!({"protocolVersion": "2024-11-05"}),
        );
        let answer = proxy.handle(init).await.unwrap();
        let result = &answer["result"];
        assert_eq!(result["protocolVersion"], "2024-11-05");
        assert_eq!(
            result["capabilities"],
            json!({"tools": {"listChanged": false}})
        );
        assert_eq!(result["serverInfo"]["name"], "rapidworkers");
        assert_eq!(result["serverInfo"]["title"], "RapidWorkers");
        assert_eq!(result["serverInfo"]["version"], VERSION);
        let instructions = result["instructions"].as_str().unwrap();
        assert!(instructions.ends_with(&format!("\n\n{}", no_key_message())));

        let init = rpc(
            "initialize",
            json!(1),
            json!({"protocolVersion": "1999-01-01"}),
        );
        let answer = proxy.handle(init).await.unwrap();
        assert_eq!(
            answer["result"]["protocolVersion"],
            DEFAULT_PROTOCOL_VERSION
        );

        let answer = proxy
            .handle(rpc("tools/list", json!(2), json!({})))
            .await
            .unwrap();
        assert_eq!(answer["result"]["tools"], spec()["tools"]);

        let call = rpc("tools/call", json!(3), json!({"name": "get_account"}));
        let answer = proxy.handle(call).await.unwrap();
        assert_eq!(answer["result"]["isError"], true);
        assert_eq!(answer["result"]["content"][0]["text"], no_key_message());

        for (method, key) in [
            ("resources/list", "resources"),
            ("resources/templates/list", "resourceTemplates"),
            ("prompts/list", "prompts"),
        ] {
            let answer = proxy
                .handle(rpc(method, json!(4), json!({})))
                .await
                .unwrap();
            assert_eq!(answer["result"], json!({key: []}));
        }

        let answer = proxy
            .handle(rpc("prompts/get", json!(5), json!({})))
            .await
            .unwrap();
        assert_eq!(answer["error"]["code"], METHOD_NOT_FOUND);
        let message = answer["error"]["message"].as_str().unwrap();
        assert!(message.starts_with("prompts/get needs RAPIDWORKERS_API_KEY. "));
    }

    #[test]
    fn retry_delay_matches_python() {
        assert_eq!(retry_delay(None, 1), Duration::from_secs(1));
        assert_eq!(retry_delay(None, 2), Duration::from_secs(2));
        assert_eq!(retry_delay(Some("abc"), 1), Duration::from_secs(1));
        assert_eq!(retry_delay(Some("3"), 1), Duration::from_secs(3));
        assert_eq!(retry_delay(Some("-3"), 1), Duration::ZERO);
        assert_eq!(retry_delay(Some("999"), 1), Duration::from_secs(30));
        assert_eq!(retry_delay(None, 50), Duration::from_secs(30));
    }

    #[test]
    fn ids_compare_like_python() {
        assert!(same_id(&json!(1), &json!(1.0)));
        assert!(same_id(&json!("a"), &json!("a")));
        assert!(!same_id(&json!(1), &json!("1")));
        assert!(same_id(&Value::Null, &Value::Null));
    }

    #[test]
    fn bundled_spec_has_every_tool() {
        let spec = spec();
        assert_eq!(spec["server"]["name"], "rapidworkers");
        assert_eq!(spec["tools"].as_array().unwrap().len(), 22);
        assert!(no_key_message().contains(API_KEYS_URL));
    }
}
