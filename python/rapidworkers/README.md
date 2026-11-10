# RapidWorkers Python SDK

Hire real people from code. [RapidWorkers](https://www.rapidworkers.io/) runs paid microtasks done by people on their own devices: AI training data collection (photos, audio, video, text), app and website testing, surveys and research, and other small tasks. You pay in USD from your account balance and only approve the work that meets your brief.

```bash
pip install rapidworkers
```

## Quick start

Create an API key at <https://www.rapidworkers.io/dashboard/api-management/> and set it as `RAPIDWORKERS_API_KEY`.

```python
from rapidworkers import RapidWorkers

rw = RapidWorkers()  # or RapidWorkers(api_key="...")

print(rw.get_account())                      # balance, deposit status, AI task access
info = rw.get_platform_info(section="ai_task_types")

# 1. Get a quote (nothing is charged)
quote = rw.preview_job_cost(kind="ai_task", pay_rate=0.25, quantity=100)
print(quote["total"], quote["expires_at"])  # show the total and get a yes first

# 2. Create the task with that quote_id once the cost is approved
task = rw.create_ai_task(
    task_type="image_collection",
    title="Photo of a paper receipt",
    description="Take a clear photo of one paper receipt from a recent purchase.",
    submission_instructions="Upload one photo. The whole receipt must be visible and readable.",
    pay_rate=0.25,
    quantity=100,
    length_in_minutes=5,
    countries=["All"],
    quote_id=quote["quote_id"],
    confirm=True,
)

# 3. Review the work
subs = rw.list_submissions(job_id=task["job_id"], kind="ai_task", status="Pending")
for s in subs["submissions"]:
    rw.approve_submission(job_id=task["job_id"], kind="ai_task", submission_id=s["submission_id"])
```

Field names in the answers follow the server; print a response to see its shape.

## What the client does for you

- **One method per tool.** Every tool of the hosted [RapidWorkers MCP server](https://www.rapidworkers.io/mcp/) is a typed method: account, platform rules, cost quotes, jobs, prefilled jobs, AI tasks, submissions and worker blocking. `rw.call_tool(name, {...})` calls any tool by name, and `rw.list_tools()` returns the live definitions.
- **Money is never moved twice.** Calls that spend or pay out (`create_*`, `extend_job`, `cancel_job`, `approve_*`, `reject_submission`) get an idempotency key automatically, and a call that times out is retried with the same key. Pass your own `idempotency_key` to control this across processes.
- **Clear errors.** A refused call raises `ToolError` whose `message` says how to fix it (for example a low balance or an expired `quote_id`). `AuthenticationError`, `RateLimitError`, `ServiceUnavailableError` and `APIConnectionError` cover the transport.
- **Async too.** `AsyncRapidWorkers` has the same methods as coroutines.

```python
import asyncio
from rapidworkers import AsyncRapidWorkers

async def main():
    async with AsyncRapidWorkers() as rw:
        print(await rw.list_jobs(status="Active"))

asyncio.run(main())
```

## Rules worth knowing

- At least $0.05 per worker ($0.10 for AI tasks) and at least 10 workers per job.
- A first deposit (minimum $20) is required before posting. `get_deposit_link` returns a checkout URL for a person to pay.
- Create calls need a `quote_id` from `preview_job_cost` and `confirm=True`.
- Approving pays the worker and cannot be undone. Pending submissions are approved automatically after 2 days.
- AI tasks need access approved by the RapidWorkers team (`request_ai_task_access`).

## Use it from an AI agent

For Claude, Cursor and other MCP clients, connect the hosted server at `https://www.rapidworkers.io/api/mcp/` directly (OAuth sign-in), or run the [`rapidworkers-mcp`](https://pypi.org/project/rapidworkers-mcp/) package with an API key.

## Links

- Docs: <https://www.rapidworkers.io/developers/>
- Source: <https://github.com/RapidWorkers-io/rapidworkers>
- License: MIT
