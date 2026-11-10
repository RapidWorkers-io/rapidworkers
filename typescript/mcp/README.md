# RapidWorkers MCP server

<!-- mcp-name: io.rapidworkers/rapidworkers -->

Let your AI agent hire real people. [RapidWorkers](https://www.rapidworkers.io/) connects agents to people who do paid microtasks on their own devices:

- **AI data collection:** photos, audio, video and text gathered to your brief (receipts, voices with different accents, objects in real homes).
- **Real-device testing:** people install your app or open your site and report what broke, with screenshots.
- **Human judgment:** native-speaker checks, surveys, first-impression and usability feedback.

The agent gets a cost quote, asks you to confirm it, posts the task, then reviews each submission against the brief and approves or rejects it. Every payment needs a quote and an explicit confirmation.

## Two ways to connect

**1. Hosted server, no install (recommended where supported).** Add `https://www.rapidworkers.io/api/mcp/` as a remote MCP server and sign in with OAuth. In Claude Code:

```bash
claude mcp add --transport http rapidworkers https://www.rapidworkers.io/api/mcp/
```

**2. This package, for clients that launch local servers.** Create an API key at <https://www.rapidworkers.io/dashboard/api-management/>, then:

```bash
npx -y rapidworkers-mcp     # Node 20+, no dependencies
```

Claude Desktop, Cursor, Windsurf, Cline and similar (`mcpServers` config):

```json
{
  "mcpServers": {
    "rapidworkers": {
      "command": "npx",
      "args": ["-y", "rapidworkers-mcp"],
      "env": { "RAPIDWORKERS_API_KEY": "your-api-key" }
    }
  }
}
```

Claude Code:

```bash
claude mcp add rapidworkers -e RAPIDWORKERS_API_KEY=your-api-key -- npx -y rapidworkers-mcp
```

The package relays every request to the hosted server, so its tools, instructions and limits are always the live ones. Without `RAPIDWORKERS_API_KEY` it still starts and lists the tools; tool calls then explain how to create a key.

| Variable | Required | Description |
| --- | --- | --- |
| `RAPIDWORKERS_API_KEY` | yes, for tool calls | API key from your RapidWorkers dashboard |
| `RAPIDWORKERS_MCP_URL` | no | Endpoint override (default `https://www.rapidworkers.io/api/mcp/`) |

## Tools

| Tool | What it does |
| --- | --- |
| `get_account` | Balance, deposit status, AI task access, commission rate |
| `get_platform_info` | Limits, fees, categories, prefilled job types, AI task types, country codes |
| `list_transactions` | Latest deposits, charges, refunds |
| `get_deposit_link` | Checkout link for a person to add funds ($20 minimum) |
| `preview_job_cost` | Cost quote with a `quote_id` (nothing is charged) |
| `create_job` | Post a custom microtask |
| `create_prefilled_job` | Post a ready-made job from a link |
| `create_ai_task` | Collect photos, audio, video or text for AI work |
| `request_ai_task_access` | Ask the RapidWorkers team to enable AI tasks |
| `list_jobs` / `get_job` | Your jobs and AI tasks, newest first |
| `extend_job` / `cancel_job` | Add workers, or cancel and refund the rest |
| `list_submissions` | Proofs to review |
| `approve_submission` / `reject_submission` / `approve_all_pending` | Pay or reject work |
| `block_worker` / `unblock_worker` / `list_blocked_workers` | Manage who can work for you |

## Rules

At least $0.05 per worker ($0.10 for AI tasks), at least 10 workers per job, $20 minimum deposit. A first deposit is required before posting. Approving pays the worker and cannot be undone; pending submissions are approved automatically after 2 days.

## Links

- Docs: <https://www.rapidworkers.io/developers/#mcp>
- JavaScript/TypeScript SDK: [`rapidworkers`](https://www.npmjs.com/package/rapidworkers)
- Also on PyPI: [`rapidworkers-mcp`](https://pypi.org/project/rapidworkers-mcp/) (`uvx rapidworkers-mcp`)
- Source: <https://github.com/RapidWorkers-io/rapidworkers>
- License: MIT
