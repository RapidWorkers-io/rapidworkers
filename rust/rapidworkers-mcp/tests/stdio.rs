//! End to end over real stdio with the built binary. Nothing reaches the real server: without a
//! key the binary answers locally, and with a key it talks to a local mock.

use std::collections::HashMap;
use std::process::{Command, Stdio};
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader, Lines};
use tokio::process::ChildStdout;
use wiremock::matchers::{body_partial_json, method};
use wiremock::{Mock, MockServer, Request, ResponseTemplate};

const BIN: &str = env!("CARGO_BIN_EXE_rapidworkers-mcp");

fn rpc(method: &str, id: Value, params: Value) -> Value {
    json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

/// Runs the binary without RAPIDWORKERS_API_KEY on `input`; returns (stdout lines, stderr).
fn run_without_key(input: &str) -> (Vec<Value>, String) {
    let mut child = Command::new(BIN)
        .env_remove("RAPIDWORKERS_API_KEY")
        // Would be used only with a key; points nowhere to be sure
        .env("RAPIDWORKERS_MCP_URL", "http://127.0.0.1:9/api/mcp/")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("start rapidworkers-mcp");
    {
        use std::io::Write;
        let mut stdin = child.stdin.take().unwrap();
        stdin.write_all(input.as_bytes()).unwrap();
    }
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success(), "{output:?}");
    let stdout = String::from_utf8(output.stdout).unwrap();
    let lines = stdout
        .lines()
        .map(|line| serde_json::from_str(line).expect("one JSON message per line"))
        .collect();
    (lines, String::from_utf8(output.stderr).unwrap())
}

fn by_id(answers: Vec<Value>) -> HashMap<String, Value> {
    answers
        .into_iter()
        .map(|answer| (answer["id"].to_string(), answer))
        .collect()
}

#[test]
fn stdio_without_key_lists_tools_and_explains_calls() {
    let init_params = json!({
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "t", "version": "1"},
    });
    let lines = [
        rpc("initialize", json!(1), init_params),
        json!({"jsonrpc": "2.0", "method": "notifications/initialized"}),
        rpc("tools/list", json!(2), json!({})),
        rpc(
            "tools/call",
            json!(3),
            json!({"name": "get_account", "arguments": {}}),
        ),
    ];
    let input: String = lines.iter().map(|line| format!("{line}\n")).collect();
    let (answers, stderr) = run_without_key(&input);
    let answers = by_id(answers);
    assert_eq!(answers.len(), 3, "{answers:?}");

    let init = &answers["1"]["result"];
    assert_eq!(init["serverInfo"]["name"], "rapidworkers");
    assert_eq!(init["serverInfo"]["title"], "RapidWorkers");
    assert_eq!(init["serverInfo"]["version"], env!("CARGO_PKG_VERSION"));
    assert_eq!(init["protocolVersion"], "2025-06-18");
    assert_eq!(
        init["capabilities"],
        json!({"tools": {"listChanged": false}})
    );
    assert!(init["instructions"]
        .as_str()
        .unwrap()
        .contains("RAPIDWORKERS_API_KEY is not set"));

    let tools = answers["2"]["result"]["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 22);
    assert!(tools.iter().any(|tool| tool["name"] == "create_ai_task"));

    let call = &answers["3"]["result"];
    assert_eq!(call["isError"], true);
    let text = call["content"][0]["text"].as_str().unwrap();
    assert!(text.contains("RAPIDWORKERS_API_KEY"));
    assert!(text.contains("https://www.rapidworkers.io/dashboard/api-management/"));

    assert!(stderr.contains("not set"), "{stderr}");
}

#[test]
fn stdio_parse_errors_batches_and_blank_lines() {
    let batch = json!([
        rpc("ping", json!("a"), json!({})),
        {"jsonrpc": "2.0", "method": "notifications/cancelled"},
        rpc("resources/list", json!("b"), json!({})),
    ]);
    let input = format!("\n   \nnot json\n{batch}\r\n{}\n", json!({"id": 7}));
    let (answers, _) = run_without_key(&input);
    assert_eq!(answers.len(), 4, "{answers:?}");
    let parse_error = answers
        .iter()
        .find(|answer| answer["error"]["code"] == -32700)
        .unwrap();
    assert_eq!(parse_error["id"], Value::Null);
    assert_eq!(parse_error["error"]["message"], "Parse error.");
    let invalid = answers
        .iter()
        .find(|answer| answer["error"]["code"] == -32600)
        .unwrap();
    assert_eq!(invalid["id"], Value::Null);
    let answers = by_id(answers);
    assert_eq!(answers["\"a\""]["result"], json!({}));
    assert_eq!(answers["\"b\""]["result"], json!({"resources": []}));
}

#[test]
fn version_and_help_flags() {
    let output = Command::new(BIN).arg("--version").output().unwrap();
    assert!(output.status.success());
    let expected = format!("rapidworkers-mcp {}\n", env!("CARGO_PKG_VERSION"));
    assert_eq!(String::from_utf8(output.stdout).unwrap(), expected);

    let output = Command::new(BIN).arg("-h").output().unwrap();
    assert!(output.status.success());
    let help = String::from_utf8(output.stdout).unwrap();
    assert!(help.contains("Usage: rapidworkers-mcp"));
    assert!(help.contains("RAPIDWORKERS_MCP_URL"));
}

/// Answers with `result` and the request's id, after `delay`.
fn echo(result: Value, delay: Duration) -> impl Fn(&Request) -> ResponseTemplate + Send + Sync {
    move |request: &Request| {
        let body: Value = serde_json::from_slice(&request.body).unwrap();
        ResponseTemplate::new(200)
            .set_body_json(json!({"jsonrpc": "2.0", "id": body["id"], "result": result}))
            .set_delay(delay)
    }
}

async fn next_answer(lines: &mut Lines<BufReader<ChildStdout>>) -> Value {
    let line = tokio::time::timeout(Duration::from_secs(10), lines.next_line())
        .await
        .expect("an answer within 10 s")
        .unwrap()
        .expect("stdout open");
    serde_json::from_str(&line).unwrap()
}

#[tokio::test(flavor = "multi_thread")]
async fn stdio_with_key_relays_concurrently() {
    let server = MockServer::start().await;
    let routes = [
        ("initialize", json!({"protocolVersion": "2025-06-18"}), 0),
        ("tools/call", json!({"slow": true}), 800),
        ("tools/list", json!({"tools": []}), 0),
    ];
    for (rpc_method, result, delay) in routes {
        Mock::given(method("POST"))
            .and(body_partial_json(json!({"method": rpc_method})))
            .respond_with(echo(result, Duration::from_millis(delay)))
            .mount(&server)
            .await;
    }

    let mut child = tokio::process::Command::new(BIN)
        .env("RAPIDWORKERS_API_KEY", " test-key ")
        .env("RAPIDWORKERS_MCP_URL", format!("{}/api/mcp/", server.uri()))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let mut stdin = child.stdin.take().unwrap();
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();

    let init = rpc(
        "initialize",
        json!(1),
        json!({"protocolVersion": "2025-06-18"}),
    );
    stdin
        .write_all(format!("{init}\n").as_bytes())
        .await
        .unwrap();
    let answer = next_answer(&mut lines).await;
    assert_eq!(answer["id"], 1);
    assert_eq!(answer["result"]["protocolVersion"], "2025-06-18");

    let slow = rpc(
        "tools/call",
        json!(2),
        json!({"name": "get_account", "arguments": {}}),
    );
    let fast = rpc("tools/list", json!(3), json!({}));
    let notification = json!({"jsonrpc": "2.0", "method": "notifications/initialized"});
    let ping = rpc("ping", json!(4), json!({}));
    let input = format!("{slow}\n{fast}\n{notification}\n{ping}\n");
    stdin.write_all(input.as_bytes()).await.unwrap();

    let mut order = Vec::new();
    for _ in 0..3 {
        let answer = next_answer(&mut lines).await;
        order.push(answer["id"].as_i64().unwrap());
        if answer["id"] == 2 {
            assert_eq!(answer["result"], json!({"slow": true}));
        }
    }
    // The slow call did not hold up the others
    assert_eq!(order.last(), Some(&2), "{order:?}");
    assert_eq!(order.len(), 3);

    drop(stdin);
    let status = tokio::time::timeout(Duration::from_secs(10), child.wait())
        .await
        .unwrap()
        .unwrap();
    assert!(status.success());

    let requests = server.received_requests().await.unwrap();
    assert_eq!(requests.len(), 3, "ping and the notification stay local");
    for request in &requests {
        assert_eq!(request.headers.get("x-api-key").unwrap(), "test-key");
        let body: Value = serde_json::from_slice(&request.body).unwrap();
        let version = request.headers.get("mcp-protocol-version");
        if body["method"] == "initialize" {
            assert!(version.is_none());
        } else {
            assert_eq!(version.unwrap(), "2025-06-18");
            let sent = if body["id"] == 2 { &slow } else { &fast };
            assert_eq!(&body, sent);
        }
    }
}
