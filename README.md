# RapidWorkers SDKs and MCP server

<!-- mcp-name: io.rapidworkers/rapidworkers -->

[RapidWorkers](https://www.rapidworkers.io/) lets software and AI agents hire real people for small paid tasks done on their own devices:

- **AI data collection:** photos, audio, video and text gathered to your brief.
- **Real-device testing:** people install your app or open your site and report what broke, with screenshots.
- **Human judgment:** native-speaker checks, surveys, usability and first-impression feedback.

You get a cost quote, confirm it, post the task, then review each submission and approve (pay) or reject it. This repository holds the official clients. Every one of them talks to the hosted MCP server at `https://www.rapidworkers.io/api/mcp/`, so they all share the same tools, validation and safeguards.

## Packages

| Language | SDK | MCP server for local clients |
| --- | --- | --- |
| Python | [`pip install rapidworkers`](python/rapidworkers) | [`uvx rapidworkers-mcp`](python/rapidworkers-mcp) |
| JavaScript / TypeScript | [`npm install rapidworkers`](typescript/rapidworkers) | [`npx -y rapidworkers-mcp`](typescript/mcp) |
| Rust | [`cargo add rapidworkers`](rust/rapidworkers) | [`cargo install rapidworkers-mcp`](rust/rapidworkers-mcp) |
| Go | [`go get github.com/RapidWorkers-io/rapidworkers/go`](go) | [`go install github.com/RapidWorkers-io/rapidworkers/go/cmd/rapidworkers-mcp@latest`](go/cmd/rapidworkers-mcp) |
| Docker | | `docker run -i --rm -e RAPIDWORKERS_API_KEY ghcr.io/rapidworkers-io/rapidworkers-mcp` |
| Claude Desktop | | One-click extension: download `rapidworkers-mcp-<version>.mcpb` from [Releases](https://github.com/RapidWorkers-io/rapidworkers/releases) and open it |

All of them need an API key from <https://www.rapidworkers.io/dashboard/api-management/>, passed as `RAPIDWORKERS_API_KEY`.

## Connect an AI agent

The easiest way is the hosted server, with OAuth sign-in and nothing to install:

```bash
claude mcp add --transport http rapidworkers https://www.rapidworkers.io/api/mcp/
```

For clients that launch local servers (Claude Desktop, Cursor, Windsurf, Cline, Zed...):

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

## Use it from code

```python
from rapidworkers import RapidWorkers

rw = RapidWorkers()
quote = rw.preview_job_cost(kind="ai_task", pay_rate=0.25, quantity=100)
print(quote["total"])  # confirm the cost before creating anything
```

```ts
import { RapidWorkers } from 'rapidworkers';

const rw = new RapidWorkers();
const quote = await rw.previewJobCost({ kind: 'ai_task', pay_rate: 0.25, quantity: 100 });
```

See each package's README for full examples.

## Rules

At least $0.05 per worker ($0.10 for AI tasks), at least 10 workers per job, and a $20 minimum deposit. Creating a job needs a `quote_id` from `preview_job_cost` and `confirm: true`. Approving pays the worker and cannot be undone; pending submissions are approved automatically after 2 days.

## How this repository works

- [`spec/tools.json`](spec/tools.json) is the exact tool list of the hosted server. [`scripts/codegen.py`](scripts/codegen.py) generates each SDK's typed methods from it; CI fails if a generated file is stale.
- The MCP packages are thin stdio relays: they forward every request unchanged to the hosted server, so they never fall behind it.
- [`server.json`](server.json) is the [MCP Registry](https://registry.modelcontextprotocol.io/) entry.
- Releases are published from GitHub Actions; see [RELEASING.md](RELEASING.md).

## License

MIT
