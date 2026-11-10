//! stdio MCP server that relays every request to the hosted RapidWorkers MCP server.
//!
//! Reads newline-delimited JSON-RPC from stdin and writes one answer per line to stdout; logs go
//! to stderr only.

mod proxy;

use std::process::ExitCode;
use std::sync::Arc;

use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::mpsc;
use tokio::task::JoinSet;

use proxy::{
    error_response, log, Proxy, API_KEYS_URL, API_KEY_ENV, BUNDLED_SPEC, INTERNAL_ERROR,
    PARSE_ERROR, URL_ENV, VERSION,
};

const HELP: &str = "\
stdio MCP server that relays every request to the hosted RapidWorkers MCP server.

Usage: rapidworkers-mcp [--version] [--help]

MCP clients that launch local servers (Claude Desktop, Cursor, Windsurf, Zed,
Cline...) talk to this process over stdin/stdout. Each JSON-RPC request is sent
unchanged to https://www.rapidworkers.io/api/mcp/ with the API key, and the
answer is written back, so the tools, instructions, prompts and resources are
always exactly the live ones.

Without RAPIDWORKERS_API_KEY the server still starts and lists the tools from a
bundled snapshot (so registries and inspectors can see them); every tool call
then explains how to create a key.
";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|arg| arg == "-V" || arg == "--version") {
        println!("rapidworkers-mcp {VERSION}");
        return ExitCode::SUCCESS;
    }
    if args.iter().any(|arg| arg == "-h" || arg == "--help") {
        println!("{HELP}");
        println!(
            "Environment: {API_KEY_ENV} (required for tool calls), {URL_ENV} (optional endpoint override)."
        );
        return ExitCode::SUCCESS;
    }

    let spec: Value = match serde_json::from_str(BUNDLED_SPEC) {
        Ok(spec) => spec,
        Err(err) => {
            log(&format!("the bundled tools.json is invalid: {err}"));
            return ExitCode::FAILURE;
        }
    };
    let api_key = env_value(API_KEY_ENV);
    let url = Some(env_value(URL_ENV))
        .filter(|url| !url.is_empty())
        .or_else(|| spec["server"]["url"].as_str().map(str::to_owned))
        .unwrap_or_default();
    if api_key.is_empty() {
        log(&format!(
            "{API_KEY_ENV} is not set: tools are listed but calls will ask for a key ({API_KEYS_URL})."
        ));
    }
    let proxy = match Proxy::new(api_key, url, spec) {
        Ok(proxy) => Arc::new(proxy),
        Err(err) => {
            log(&format!("could not create the HTTP client: {err}"));
            return ExitCode::FAILURE;
        }
    };
    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(err) => {
            log(&format!("could not start: {err}"));
            return ExitCode::FAILURE;
        }
    };
    runtime.block_on(serve(proxy));
    ExitCode::SUCCESS
}

fn env_value(name: &str) -> String {
    std::env::var(name).unwrap_or_default().trim().to_owned()
}

/// Reads stdin until it closes, answering each line concurrently; answers are written one line
/// at a time by a single writer.
async fn serve(proxy: Arc<Proxy>) {
    let (out, mut answers) = mpsc::unbounded_channel::<Value>();
    let writer = tokio::spawn(async move {
        let mut stdout = tokio::io::stdout();
        while let Some(answer) = answers.recv().await {
            let mut line = answer.to_string().into_bytes();
            line.push(b'\n');
            if stdout.write_all(&line).await.is_err() || stdout.flush().await.is_err() {
                log("stdout is closed");
                break;
            }
        }
    });

    let mut stdin = BufReader::new(tokio::io::stdin());
    let mut pending = JoinSet::new();
    loop {
        let mut line = Vec::new();
        match stdin.read_until(b'\n', &mut line).await {
            Ok(0) => break,
            Ok(_) => {}
            Err(err) => {
                log(&format!("could not read stdin: {err}"));
                break;
            }
        }
        // Forget finished tasks so a long session does not keep them all
        while pending.try_join_next().is_some() {}
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        pending.spawn(answer(Arc::clone(&proxy), line, out.clone()));
    }
    while pending.join_next().await.is_some() {}
    drop(out);
    let _ = writer.await;
}

/// Answers one stdin line: a message or a batch, each item answered on its own line.
async fn answer(proxy: Arc<Proxy>, line: Vec<u8>, out: mpsc::UnboundedSender<Value>) {
    let message: Value = match serde_json::from_slice(&line) {
        Ok(message) => message,
        Err(_) => {
            let code = Value::from(PARSE_ERROR);
            let _ = out.send(error_response(Value::Null, code, "Parse error.", None));
            return;
        }
    };
    let items = match message {
        Value::Array(items) => items,
        other => vec![other],
    };
    for item in items {
        let request_id = item.get("id").cloned().unwrap_or(Value::Null);
        let proxy = Arc::clone(&proxy);
        // A panic while answering one request must not stop the server
        let reply = match tokio::spawn(async move { proxy.handle(item).await }).await {
            Ok(reply) => reply,
            Err(err) => {
                log(&format!("internal error: {err}"));
                let code = Value::from(INTERNAL_ERROR);
                let text = "Internal error in rapidworkers-mcp.";
                Some(error_response(request_id, code, text, None))
            }
        };
        if let Some(reply) = reply {
            let _ = out.send(reply);
        }
    }
}
