# RapidWorkers Rust SDK

Hire real people from code. [RapidWorkers](https://www.rapidworkers.io/) runs paid microtasks done by people on their own devices: AI training data collection (photos, audio, video, text), app and website testing, surveys and research, and other small tasks. You pay in USD from your account balance and only approve the work that meets your brief.

```bash
cargo add rapidworkers
cargo add tokio --features macros,rt-multi-thread
```

## Quick start

Create an API key at <https://www.rapidworkers.io/dashboard/api-management/> and set it as `RAPIDWORKERS_API_KEY`.

```rust,no_run
use rapidworkers::{
    AiTaskType, ApproveSubmissionParams, Client, CreateAiTaskParams, GetPlatformInfoParams,
    JobKind, ListSubmissionsParams, PlatformInfoSection, PreviewJobCostParams, SubmissionStatus,
};

#[tokio::main]
async fn main() -> rapidworkers::Result<()> {
    let rw = Client::from_env()?; // or Client::new("...")?

    println!("{}", rw.get_account().await?); // balance, deposit status, AI task access
    let section = PlatformInfoSection::AiTaskTypes;
    let info = rw
        .get_platform_info(GetPlatformInfoParams::new().section(section))
        .await?;
    println!("{info:#}");

    // 1. Get a quote (nothing is charged)
    let quote = rw
        .preview_job_cost(PreviewJobCostParams::new(0.25, 100).kind(JobKind::AiTask))
        .await?;
    println!("{} {}", quote["total"], quote["expires_at"]); // show the total and get a yes first

    // 2. Create the task with that quote_id once the cost is approved
    let task = rw
        .create_ai_task(CreateAiTaskParams::new(
            AiTaskType::ImageCollection,
            "Photo of a paper receipt",
            "Take a clear photo of one paper receipt from a recent purchase.",
            "Upload one photo. The whole receipt must be visible and readable.",
            0.25,    // pay_rate
            100,     // quantity
            5,       // length_in_minutes
            ["All"], // countries
            quote["quote_id"].as_str().unwrap_or_default(),
            true, // confirm
        ))
        .await?;
    let job_id = task["job_id"].as_str().unwrap_or_default();

    // 3. Review the work
    let subs = rw
        .list_submissions(
            ListSubmissionsParams::new(job_id)
                .kind(JobKind::AiTask)
                .status(SubmissionStatus::Pending),
        )
        .await?;
    for s in subs["submissions"].as_array().into_iter().flatten() {
        let submission_id = s["submission_id"].as_u64().unwrap_or_default();
        let approve = ApproveSubmissionParams::new(job_id, submission_id).kind(JobKind::AiTask);
        rw.approve_submission(approve).await?;
    }
    Ok(())
}
```

Each tool takes a `<Tool>Params` value: `new(...)` takes the required arguments in order, and a method per optional argument sets it. Answers are `serde_json::Value`s whose field names follow the server; print one to see its shape.

## What the client does for you

- **One method per tool.** Every tool of the hosted [RapidWorkers MCP server](https://www.rapidworkers.io/mcp/) is an async method on `Client`: account, platform rules, cost quotes, jobs, prefilled jobs, AI tasks, submissions and worker blocking. `rw.call_tool(name, json!({...}))` calls any tool by name, and `rw.list_tools()` returns the live definitions.
- **Money is never moved twice.** Calls that spend or pay out (`create_*`, `extend_job`, `cancel_job`, `approve_*`, `reject_submission`) get an idempotency key automatically, and a call that times out is retried with the same key. Set your own with `.idempotency_key(...)` to control this across processes.
- **Clear errors.** A refused call returns `Error::Tool`, whose `message` says how to fix it (for example a low balance or an expired `quote_id`). `Error::Authentication`, `Error::RateLimit`, `Error::ServiceUnavailable`, `Error::Api` and `Error::Connection` cover the transport.
- **Retries that are safe.** HTTP 429 and 503 answers are retried for every call. Network errors and HTTP 502/504 are retried only for read-only or idempotent tools and for calls with an idempotency key. `Client::builder()` sets the endpoint, the timeout and `max_retries` (default 2).

```rust,no_run
use std::time::Duration;

use rapidworkers::{Client, Error, PreviewJobCostParams};

async fn quote() -> rapidworkers::Result<()> {
    let rw = Client::builder()
        .api_key("your-api-key")
        .timeout(Duration::from_secs(30))
        .build()?;
    match rw.preview_job_cost(PreviewJobCostParams::new(0.01, 10)).await {
        Ok(quote) => println!("total {}", quote["total"]),
        Err(Error::Tool { message, details, .. }) => eprintln!("refused: {message} {details:?}"),
        Err(other) => return Err(other),
    }
    Ok(())
}
```

## Rules worth knowing

- At least $0.05 per worker ($0.10 for AI tasks, $0.50 per survey response) and at least 10 workers per job.
- A first deposit (minimum $20) is required before posting. `get_deposit_link` returns a checkout URL for a person to pay.
- Create calls need a `quote_id` from `preview_job_cost` and `confirm` set to `true`.
- Approving pays the worker and cannot be undone. Pending submissions are approved automatically after 2 days.
- AI tasks need access approved by the RapidWorkers team (`request_ai_task_access`).

## Use it from an AI agent

For Claude, Cursor and other MCP clients, connect the hosted server at `https://www.rapidworkers.io/api/mcp/` directly (OAuth sign-in), or run the [`rapidworkers-mcp`](https://crates.io/crates/rapidworkers-mcp) server with an API key (`cargo install rapidworkers-mcp`).

## Links

- Docs: <https://www.rapidworkers.io/developers/>
- API reference: <https://docs.rs/rapidworkers>
- Source: <https://github.com/RapidWorkers-io/rapidworkers>
- License: MIT
