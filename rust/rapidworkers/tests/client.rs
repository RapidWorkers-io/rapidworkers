//! The client's HTTP behaviour against a local mock of the MCP endpoint.

mod common;

use std::collections::HashSet;
use std::time::Duration;

use common::{reply, structured, Mock, API_KEY};
use rapidworkers::{
    ApproveSubmissionParams, CancelJobParams, Client, Error, GetDepositLinkParams, JobKind,
    ListJobsParams, PreviewJobCostParams,
};
use serde_json::{json, Value};
use wiremock::{Request, ResponseTemplate};

const JOB_ID: &str = "00000000-0000-0000-0000-000000000000";

#[tokio::test]
async fn tool_call_sends_jsonrpc_and_returns_structured_content() {
    let mock = Mock::start().await;
    mock.answer(reply(structured(json!({"balance": "12.50"}))))
        .await;
    let answer = mock.client().get_account().await.unwrap();
    assert_eq!(answer, json!({"balance": "12.50"}));

    let requests = mock.requests().await;
    assert_eq!(requests.len(), 1);
    let request = &requests[0];
    let body: Value = serde_json::from_slice(&request.body).unwrap();
    assert_eq!(body["jsonrpc"], "2.0");
    assert!(body["id"].is_u64());
    assert_eq!(body["method"], "tools/call");
    assert_eq!(
        body["params"],
        json!({"name": "get_account", "arguments": {}})
    );
    let header = |name: &str| request.headers.get(name).unwrap().to_str().unwrap();
    assert_eq!(header("x-api-key"), API_KEY);
    assert_eq!(header("content-type"), "application/json");
    assert_eq!(header("accept"), "application/json, text/event-stream");
    assert_eq!(header("mcp-protocol-version"), "2025-06-18");
    assert_eq!(
        header("user-agent"),
        format!("rapidworkers-rust/{}", rapidworkers::VERSION)
    );
}

#[tokio::test]
async fn unset_arguments_are_dropped() {
    let mock = Mock::start().await;
    mock.answer(reply(structured(json!({"jobs": []})))).await;
    let params = ListJobsParams::new().kind(JobKind::AiTask);
    mock.client().list_jobs(params).await.unwrap();
    let arguments = mock.tool_arguments("list_jobs").await;
    assert_eq!(Value::Object(arguments), json!({"kind": "ai_task"}));
}

#[tokio::test]
async fn call_tool_drops_null_arguments() {
    let mock = Mock::start().await;
    mock.answer(reply(structured(json!({"jobs": []})))).await;
    let arguments = json!({"kind": "job", "status": null, "search": null});
    mock.client()
        .call_tool("list_jobs", arguments)
        .await
        .unwrap();
    let sent = mock.tool_arguments("list_jobs").await;
    assert_eq!(Value::Object(sent), json!({"kind": "job"}));
}

#[tokio::test]
async fn money_calls_get_an_idempotency_key_reused_on_retry() {
    let mock = Mock::start().await;
    // 1. no answer in time, 2. gateway timeout, 3. success
    let slow = ResponseTemplate::new(200).set_delay(Duration::from_secs(10));
    mock.answer_times(slow, Some(1)).await;
    let gateway = ResponseTemplate::new(504).insert_header("Retry-After", "0");
    mock.answer_times(gateway, Some(1)).await;
    mock.answer(reply(structured(json!({"approved": true}))))
        .await;

    let client = Client::builder()
        .api_key(API_KEY)
        .base_url(mock.url())
        .timeout(Duration::from_millis(300))
        .build()
        .unwrap();
    let params = ApproveSubmissionParams::new(JOB_ID, 5);
    let answer = client.approve_submission(params).await.unwrap();
    assert_eq!(answer, json!({"approved": true}));

    let bodies = mock.bodies().await;
    assert_eq!(bodies.len(), 3);
    let keys: HashSet<&str> = bodies
        .iter()
        .map(|body| {
            body["params"]["arguments"]["idempotency_key"]
                .as_str()
                .unwrap()
        })
        .collect();
    assert_eq!(keys.len(), 1, "one key for every attempt: {keys:?}");
    let key = keys.into_iter().next().unwrap();
    assert!(key.starts_with("sdk-") && key.len() == 36, "{key}");
    // Each attempt is its own JSON-RPC request
    let ids: HashSet<u64> = bodies.iter().map(|b| b["id"].as_u64().unwrap()).collect();
    assert_eq!(ids.len(), 3);
}

#[tokio::test]
async fn each_call_gets_a_new_idempotency_key() {
    let mock = Mock::start().await;
    mock.answer(reply(structured(json!({"ok": true})))).await;
    let client = mock.client();
    for _ in 0..2 {
        let params = CancelJobParams::new(JOB_ID, true);
        client.cancel_job(params).await.unwrap();
    }
    let keys: HashSet<String> = mock
        .bodies()
        .await
        .iter()
        .map(|b| b["params"]["arguments"]["idempotency_key"].to_string())
        .collect();
    assert_eq!(keys.len(), 2);
}

#[tokio::test]
async fn explicit_idempotency_key_is_kept() {
    let mock = Mock::start().await;
    mock.answer(reply(structured(json!({"ok": true})))).await;
    let params = CancelJobParams::new(JOB_ID, true).idempotency_key("mine-1");
    mock.client().cancel_job(params).await.unwrap();
    let arguments = mock.tool_arguments("cancel_job").await;
    assert_eq!(arguments["idempotency_key"], "mine-1");
    assert_eq!(arguments["confirm"], true);
}

#[tokio::test]
async fn non_idempotent_write_is_not_retried_after_gateway_error() {
    let mock = Mock::start().await;
    let gateway = ResponseTemplate::new(504)
        .set_body_string("gateway timeout")
        .insert_header("Retry-After", "0");
    mock.answer(gateway).await;
    let error = mock
        .client()
        .get_deposit_link(GetDepositLinkParams::new(20.0))
        .await
        .unwrap_err();
    match &error {
        Error::ServiceUnavailable {
            status, message, ..
        } => {
            assert_eq!(*status, 504);
            assert_eq!(message, "gateway timeout");
        }
        other => panic!("unexpected {other:?}"),
    }
    assert_eq!(error.status(), Some(504));
    assert_eq!(mock.requests().await.len(), 1);
}

#[tokio::test]
async fn non_idempotent_write_is_not_retried_after_network_error() {
    let mock = Mock::start().await;
    let slow = ResponseTemplate::new(200).set_delay(Duration::from_secs(10));
    mock.answer(slow).await;
    let client = Client::builder()
        .api_key(API_KEY)
        .base_url(mock.url())
        .timeout(Duration::from_millis(200))
        .build()
        .unwrap();
    let error = client
        .get_deposit_link(GetDepositLinkParams::new(20.0))
        .await
        .unwrap_err();
    assert!(matches!(error, Error::Connection { .. }), "{error:?}");
    assert!(error.is_timeout());
    assert!(error
        .to_string()
        .starts_with("Could not reach RapidWorkers: "));
    assert_eq!(mock.requests().await.len(), 1);
}

#[tokio::test]
async fn read_only_call_is_retried_after_gateway_errors_until_max_retries() {
    let mock = Mock::start().await;
    let bad_gateway = ResponseTemplate::new(502).insert_header("Retry-After", "0");
    mock.answer(bad_gateway).await;
    let error = mock.client().list_blocked_workers().await.unwrap_err();
    assert!(matches!(
        error,
        Error::ServiceUnavailable { status: 502, .. }
    ));
    assert_eq!(mock.requests().await.len(), 3);
}

#[tokio::test]
async fn status_429_is_retried_for_any_tool() {
    let mock = Mock::start().await;
    let limited = ResponseTemplate::new(429).insert_header("Retry-After", "0");
    mock.answer_times(limited, Some(1)).await;
    mock.answer(reply(structured(json!({"url": "https://pay"}))))
        .await;
    let answer = mock
        .client()
        .get_deposit_link(GetDepositLinkParams::new(20.0))
        .await
        .unwrap();
    assert_eq!(answer, json!({"url": "https://pay"}));
    assert_eq!(mock.requests().await.len(), 2);
}

#[tokio::test]
async fn status_503_is_retried_for_any_tool() {
    let mock = Mock::start().await;
    let unavailable = ResponseTemplate::new(503).insert_header("Retry-After", "0");
    mock.answer_times(unavailable, Some(1)).await;
    mock.answer(reply(structured(json!({"url": "https://pay"}))))
        .await;
    let params = GetDepositLinkParams::new(20.0);
    mock.client().get_deposit_link(params).await.unwrap();
    assert_eq!(mock.requests().await.len(), 2);
}

#[tokio::test]
async fn rate_limit_error_carries_retry_after() {
    let mock = Mock::start().await;
    let limited = ResponseTemplate::new(429)
        .insert_header("Retry-After", "7")
        .set_body_json(json!({"jsonrpc": "2.0", "id": null, "error": {"code": -32029, "message": "Slow down."}}));
    mock.answer(limited).await;
    let client = Client::builder()
        .api_key(API_KEY)
        .base_url(mock.url())
        .max_retries(0)
        .build()
        .unwrap();
    match client.get_account().await.unwrap_err() {
        Error::RateLimit {
            status,
            code,
            message,
            retry_after,
            ..
        } => {
            assert_eq!(status, 429);
            assert_eq!(code, Some(-32029));
            assert_eq!(message, "Slow down.");
            assert_eq!(retry_after, Some(Duration::from_secs(7)));
        }
        other => panic!("unexpected {other:?}"),
    }
    assert_eq!(mock.requests().await.len(), 1);
}

#[tokio::test]
async fn tool_error_carries_message_and_details() {
    let mock = Mock::start().await;
    mock.answer(reply(json!({
        "content": [{"type": "text", "text": "Balance too low."}],
        "structuredContent": {"error": {"message": "Balance too low.", "code": "insufficient_balance", "fields": ["pay_rate"]}},
        "isError": true,
    })))
    .await;
    let error = mock
        .client()
        .preview_job_cost(PreviewJobCostParams::new(0.05, 10))
        .await
        .unwrap_err();
    assert_eq!(error.tool_code(), Some("insufficient_balance"));
    assert_eq!(error.to_string(), "Balance too low.");
    match error {
        Error::Tool {
            tool,
            message,
            details,
        } => {
            assert_eq!(tool, "preview_job_cost");
            assert_eq!(message, "Balance too low.");
            let expected = json!({"code": "insufficient_balance", "fields": ["pay_rate"]});
            assert_eq!(Value::Object(*details), expected);
        }
        other => panic!("unexpected {other:?}"),
    }
}

#[tokio::test]
async fn status_401_is_an_authentication_error() {
    let mock = Mock::start().await;
    let denied = ResponseTemplate::new(401).set_body_json(json!({
        "jsonrpc": "2.0",
        "id": null,
        "error": {"code": -32001, "message": "Invalid token."},
    }));
    mock.answer(denied).await;
    let error = mock.client().get_account().await.unwrap_err();
    match &error {
        Error::Authentication {
            status,
            code,
            message,
            ..
        } => {
            assert_eq!(*status, 401);
            assert_eq!(*code, Some(-32001));
            assert!(message.starts_with("Invalid token."), "{message}");
            assert!(message.ends_with(
                "Check your API key or create one at https://www.rapidworkers.io/dashboard/api-management/"
            ));
        }
        other => panic!("unexpected {other:?}"),
    }
    assert_eq!(mock.requests().await.len(), 1);
}

#[tokio::test]
async fn other_http_errors_use_the_body() {
    let mock = Mock::start().await;
    mock.answer(ResponseTemplate::new(400).set_body_string("Bad request body"))
        .await;
    let error = mock.client().get_account().await.unwrap_err();
    assert!(matches!(
        &error,
        Error::Api { status: 400, code: None, message, .. } if message == "Bad request body"
    ));

    let mock = Mock::start().await;
    mock.answer(ResponseTemplate::new(500)).await;
    let error = mock.client().get_account().await.unwrap_err();
    assert_eq!(error.to_string(), "HTTP 500");
}

#[tokio::test]
async fn jsonrpc_error_with_status_200_is_an_api_error() {
    let mock = Mock::start().await;
    mock.answer(|request: &Request| {
        let body: Value = serde_json::from_slice(&request.body).unwrap();
        ResponseTemplate::new(200).set_body_json(json!({
            "jsonrpc": "2.0",
            "id": body["id"],
            "error": {"code": -32602, "message": "Unknown tool: nope", "data": {"tool": "nope"}},
        }))
    })
    .await;
    let error = mock
        .client()
        .call_tool("nope", json!({}))
        .await
        .unwrap_err();
    match error {
        Error::Api {
            status,
            code,
            message,
            data,
        } => {
            assert_eq!(status, 200);
            assert_eq!(code, Some(-32602));
            assert_eq!(message, "Unknown tool: nope");
            assert_eq!(data.as_deref(), Some(&json!({"tool": "nope"})));
        }
        other => panic!("unexpected {other:?}"),
    }
}

#[tokio::test]
async fn unreadable_answer_is_an_api_error() {
    let mock = Mock::start().await;
    mock.answer(ResponseTemplate::new(200).set_body_string("<html>"))
        .await;
    let error = mock.client().get_account().await.unwrap_err();
    assert!(matches!(error, Error::Api { status: 200, .. }));
    assert_eq!(error.to_string(), "Unreadable answer from RapidWorkers.");
}

#[tokio::test]
async fn sse_answer_is_parsed() {
    let mock = Mock::start().await;
    mock.answer(|request: &Request| {
        let body: Value = serde_json::from_slice(&request.body).unwrap();
        let other = json!({"jsonrpc": "2.0", "id": 999_999, "result": structured(json!({"n": 0}))});
        let message = json!({"jsonrpc": "2.0", "id": body["id"], "result": structured(json!({"n": 1}))});
        let stream = format!(
            ": keep-alive\r\n\r\nevent: message\ndata: not json\n\ndata: {other}\n\nevent: message\ndata: {message}\n\n"
        );
        ResponseTemplate::new(200).set_body_raw(stream, "text/event-stream")
    })
    .await;
    let answer = mock.client().list_blocked_workers().await.unwrap();
    assert_eq!(answer, json!({"n": 1}));
}

#[tokio::test]
async fn sse_answer_without_a_matching_id_is_unreadable() {
    let mock = Mock::start().await;
    let stream = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":424242,\"result\":{}}\n\n";
    mock.answer(ResponseTemplate::new(200).set_body_raw(stream, "text/event-stream"))
        .await;
    let error = mock.client().list_blocked_workers().await.unwrap_err();
    assert_eq!(error.to_string(), "Unreadable answer from RapidWorkers.");
}

#[tokio::test]
async fn batch_answer_is_matched_by_id() {
    let mock = Mock::start().await;
    mock.answer(|request: &Request| {
        let body: Value = serde_json::from_slice(&request.body).unwrap();
        ResponseTemplate::new(200).set_body_json(json!([
            {"jsonrpc": "2.0", "id": 0, "result": structured(json!({"n": 0}))},
            {"jsonrpc": "2.0", "id": body["id"], "result": structured(json!({"n": 2}))},
        ]))
    })
    .await;
    let answer = mock.client().list_transactions().await.unwrap();
    assert_eq!(answer, json!({"n": 2}));
}

#[tokio::test]
async fn text_only_result_is_parsed() {
    let mock = Mock::start().await;
    mock.answer(reply(json!({
        "content": [{"type": "text", "text": "One job.\n\n{\"jobs\": [1]}"}],
    })))
    .await;
    let answer = mock
        .client()
        .list_jobs(ListJobsParams::new())
        .await
        .unwrap();
    assert_eq!(answer, json!({"jobs": [1]}));
}

#[tokio::test]
async fn list_tools_returns_the_definitions() {
    let mock = Mock::start().await;
    mock.answer(reply(json!({"tools": [{"name": "get_account"}]})))
        .await;
    let tools = mock.client().list_tools().await.unwrap();
    assert_eq!(tools, vec![json!({"name": "get_account"})]);
    let bodies = mock.bodies().await;
    assert_eq!(bodies[0]["method"], "tools/list");
    assert_eq!(bodies[0]["params"], json!({}));
}

#[tokio::test]
async fn request_returns_the_raw_result() {
    let mock = Mock::start().await;
    mock.answer(reply(json!({"prompts": []}))).await;
    let result = mock
        .client()
        .request("prompts/list", Value::Null)
        .await
        .unwrap();
    assert_eq!(result, json!({"prompts": []}));
}

#[tokio::test]
async fn connection_errors_are_retried_for_safe_calls_then_reported() {
    // A port with nothing listening: bind, read the address, close
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/api/mcp/", listener.local_addr().unwrap());
    drop(listener);
    let client = Client::builder()
        .api_key(API_KEY)
        .base_url(url)
        .max_retries(1)
        .build()
        .unwrap();
    let started = std::time::Instant::now();
    let error = client.get_account().await.unwrap_err();
    assert!(matches!(error, Error::Connection { .. }), "{error:?}");
    assert!(!error.is_timeout());
    // One retry after the 0.5 s backoff
    assert!(started.elapsed() >= Duration::from_millis(450));
}

#[test]
fn every_spec_tool_has_generated_metadata() {
    // Runs inside the repository; a packaged crate has no spec/ next to it
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../../spec/tools.json");
    let Ok(text) = std::fs::read_to_string(path) else {
        eprintln!("skipped: {path} not found");
        return;
    };
    let spec: Value = serde_json::from_str(&text).unwrap();
    assert_eq!(spec["server"]["url"], rapidworkers::SERVER_URL);
    let tools = spec["tools"].as_array().unwrap();
    let names: Vec<&str> = tools.iter().map(|t| t["name"].as_str().unwrap()).collect();
    assert_eq!(names, rapidworkers::TOOL_NAMES);
    for tool in tools {
        let name = tool["name"].as_str().unwrap();
        let meta = rapidworkers::tool_meta(name).unwrap();
        let hint = |key: &str| tool["annotations"][key].as_bool().unwrap_or(false);
        let has_key = tool["inputSchema"]["properties"]
            .get("idempotency_key")
            .is_some();
        assert_eq!(meta.read_only, hint("readOnlyHint"), "{name}");
        assert_eq!(meta.idempotency_key, has_key, "{name}");
        let retry = hint("readOnlyHint") || hint("idempotentHint") || has_key;
        assert_eq!(meta.retry, retry, "{name}");
    }
    assert!(rapidworkers::tool_meta("no_such_tool").is_none());
}
