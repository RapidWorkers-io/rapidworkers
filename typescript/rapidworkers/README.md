# RapidWorkers JavaScript / TypeScript SDK

Hire real people from code. [RapidWorkers](https://www.rapidworkers.io/) runs paid microtasks done by people on their own devices: AI training data collection (photos, audio, video, text), app and website testing, surveys and research, and other small tasks. You pay in USD from your account balance and only approve the work that meets your brief.

```bash
npm install rapidworkers
```

Node 20+ (uses the built-in `fetch`). No runtime dependencies. Types included.

## Quick start

Create an API key at <https://www.rapidworkers.io/dashboard/api-management/> and set it as `RAPIDWORKERS_API_KEY`.

```ts
import { RapidWorkers } from 'rapidworkers';

const rw = new RapidWorkers(); // or new RapidWorkers({ apiKey: '...' })

console.log(await rw.getAccount()); // balance, deposit status, AI task access

// 1. Get a quote (nothing is charged)
const quote = await rw.previewJobCost({ kind: 'ai_task', pay_rate: 0.25, quantity: 100 });
console.log(quote.total, quote.expires_at); // show the total and get a yes first

// 2. Create the task with that quote_id once the cost is approved
const task = await rw.createAiTask({
    task_type: 'image_collection',
    title: 'Photo of a paper receipt',
    description: 'Take a clear photo of one paper receipt from a recent purchase.',
    submission_instructions: 'Upload one photo. The whole receipt must be visible and readable.',
    pay_rate: 0.25,
    quantity: 100,
    length_in_minutes: 5,
    countries: ['All'],
    quote_id: quote.quote_id,
    confirm: true,
});

// 3. Review the work
const { submissions } = await rw.listSubmissions({ job_id: task.job_id, kind: 'ai_task', status: 'Pending' });
for (const s of submissions) {
    await rw.approveSubmission({ job_id: task.job_id, kind: 'ai_task', submission_id: s.submission_id });
}
```

Method names are camelCase (`createAiTask`); arguments and answers use the server's snake_case field names.

## What the client does for you

- **One method per tool.** Every tool of the hosted [RapidWorkers MCP server](https://www.rapidworkers.io/mcp/) is a typed method: account, platform rules, cost quotes, jobs, prefilled jobs, AI tasks, submissions and worker blocking. `rw.callTool(name, args)` calls any tool by name, and `rw.listTools()` returns the live definitions.
- **Money is never moved twice.** Calls that spend or pay out (`create*`, `extendJob`, `cancelJob`, `approve*`, `rejectSubmission`) get an idempotency key automatically, and a call that times out is retried with the same key. Pass your own `idempotency_key` to control this across processes.
- **Clear errors.** A refused call rejects with `ToolError`, whose `message` says how to fix it (for example a low balance or an expired `quote_id`). `AuthenticationError`, `RateLimitError`, `ServiceUnavailableError` and `APIConnectionError` cover the transport.

```ts
import { ToolError } from 'rapidworkers';

try {
    await rw.createJob({ /* ... */ });
} catch (error) {
    if (error instanceof ToolError) console.error(error.message, error.details);
    else throw error;
}
```

## Rules worth knowing

- At least $0.05 per worker ($0.10 for AI tasks, $0.50 per survey response) and at least 10 workers per job.
- A first deposit (minimum $20) is required before posting. `getDepositLink` returns a checkout URL for a person to pay.
- Create calls need a `quote_id` from `previewJobCost` and `confirm: true`.
- Approving pays the worker and cannot be undone. Pending submissions are approved automatically after 2 days.
- AI tasks need access approved by the RapidWorkers team (`requestAiTaskAccess`).

## Use it from an AI agent

For Claude, Cursor and other MCP clients, connect the hosted server at `https://www.rapidworkers.io/api/mcp/` directly (OAuth sign-in), or run [`rapidworkers-mcp`](https://www.npmjs.com/package/rapidworkers-mcp) with an API key: `npx -y rapidworkers-mcp`.

## Links

- Docs: <https://www.rapidworkers.io/developers/>
- Source: <https://github.com/RapidWorkers-io/rapidworkers>
- Other languages: [Python](https://pypi.org/project/rapidworkers/), [Rust](https://crates.io/crates/rapidworkers), [Go](https://pkg.go.dev/github.com/RapidWorkers-io/rapidworkers/go)
- License: MIT
