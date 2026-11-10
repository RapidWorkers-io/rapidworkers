//! A local mock of the MCP endpoint. Tests never reach the real server.

#![allow(dead_code)]

use rapidworkers::Client;
use serde_json::{json, Map, Value};
use wiremock::matchers::{method, path};
use wiremock::{Mock as Route, MockServer, Request, Respond, ResponseTemplate};

pub const PATH: &str = "/api/mcp/";
pub const API_KEY: &str = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk";

pub struct Mock {
    pub server: MockServer,
}

impl Mock {
    pub async fn start() -> Self {
        Self {
            server: MockServer::start().await,
        }
    }

    pub fn url(&self) -> String {
        format!("{}{PATH}", self.server.uri())
    }

    pub fn client(&self) -> Client {
        Client::builder()
            .api_key(API_KEY)
            .base_url(self.url())
            .max_retries(2)
            .build()
            .expect("client")
    }

    /// Answer every POST to the endpoint with `responder`.
    pub async fn answer(&self, responder: impl Respond + 'static) {
        self.answer_times(responder, None).await;
    }

    /// Answer the next `times` POSTs (or every POST) with `responder`. Earlier routes win.
    pub async fn answer_times(&self, responder: impl Respond + 'static, times: Option<u64>) {
        let route = Route::given(method("POST"))
            .and(path(PATH))
            .respond_with(responder);
        let route = match times {
            Some(times) => route.up_to_n_times(times),
            None => route,
        };
        route.mount(&self.server).await;
    }

    pub async fn requests(&self) -> Vec<Request> {
        self.server.received_requests().await.unwrap_or_default()
    }

    pub async fn bodies(&self) -> Vec<Value> {
        self.requests()
            .await
            .iter()
            .map(|request| serde_json::from_slice(&request.body).expect("JSON body"))
            .collect()
    }

    /// The arguments of the one `tools/call` request the server received, after checking it.
    pub async fn tool_arguments(&self, tool: &str) -> Map<String, Value> {
        let bodies = self.bodies().await;
        assert_eq!(bodies.len(), 1, "expected one request, got {bodies:?}");
        let body = &bodies[0];
        assert_eq!(body["jsonrpc"], "2.0");
        assert_eq!(body["method"], "tools/call");
        assert_eq!(body["params"]["name"], tool);
        match &body["params"]["arguments"] {
            Value::Object(arguments) => arguments.clone(),
            other => panic!("arguments are not an object: {other}"),
        }
    }
}

/// A JSON-RPC answer carrying `result`, with the id of the request it answers.
pub fn reply(result: Value) -> impl Fn(&Request) -> ResponseTemplate + Send + Sync {
    move |request: &Request| {
        let body: Value = serde_json::from_slice(&request.body).expect("JSON body");
        ResponseTemplate::new(200).set_body_json(json!({
            "jsonrpc": "2.0",
            "id": body["id"],
            "result": result,
        }))
    }
}

/// A tool result as the server sends it: a text summary and the structured data.
pub fn structured(data: Value) -> Value {
    json!({
        "content": [{"type": "text", "text": format!("summary\n\n{data}")}],
        "structuredContent": data,
    })
}
