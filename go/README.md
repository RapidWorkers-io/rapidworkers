# RapidWorkers Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/RapidWorkers-io/rapidworkers/go.svg)](https://pkg.go.dev/github.com/RapidWorkers-io/rapidworkers/go)

Hire real people from code. [RapidWorkers](https://www.rapidworkers.io/) runs paid microtasks done by people on their own devices: AI training data collection (photos, audio, video, text), app and website testing, surveys and research, and other small tasks. You pay in USD from your account balance and only approve the work that meets your brief.

```bash
go get github.com/RapidWorkers-io/rapidworkers/go
```

Go 1.22 or later, standard library only.

## Quick start

Create an API key at <https://www.rapidworkers.io/dashboard/api-management/> and set it as `RAPIDWORKERS_API_KEY`.

```go
package main

import (
	"context"
	"fmt"
	"log"

	rapidworkers "github.com/RapidWorkers-io/rapidworkers/go"
)

func main() {
	ctx := context.Background()
	check := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}

	rw, err := rapidworkers.NewClientFromEnv() // or rapidworkers.NewClient("...")
	check(err)

	account, err := rw.GetAccount(ctx) // balance, deposit status, AI task access
	check(err)
	fmt.Println(account)
	info, err := rw.GetPlatformInfo(ctx, rapidworkers.GetPlatformInfoParams{
		Section: rapidworkers.PlatformInfoSectionAITaskTypes,
	})
	check(err)
	fmt.Println(info)

	// 1. Get a quote (nothing is charged)
	quote, err := rw.PreviewJobCost(ctx, rapidworkers.PreviewJobCostParams{
		Kind:     rapidworkers.KindAITask,
		PayRate:  0.25,
		Quantity: 100,
	})
	check(err)
	fmt.Println(quote["total"], quote["expires_at"]) // show the total and get a yes first

	// 2. Create the task with that quote_id once the cost is approved
	task, err := rw.CreateAITask(ctx, rapidworkers.CreateAITaskParams{
		TaskType:               rapidworkers.AITaskTypeImageCollection,
		Title:                  "Photo of a paper receipt",
		Description:            "Take a clear photo of one paper receipt from a recent purchase.",
		SubmissionInstructions: "Upload one photo. The whole receipt must be visible and readable.",
		RequiredFileCount:      rapidworkers.Int(1),
		PayRate:                0.25,
		Quantity:               100,
		LengthInMinutes:        5,
		Countries:              []string{"All"},
		QuoteID:                quote["quote_id"].(string),
		Confirm:                true,
	})
	check(err)
	jobID := task["job_id"].(string)

	// 3. Review the work
	subs, err := rw.ListSubmissions(ctx, rapidworkers.ListSubmissionsParams{
		JobID:  jobID,
		Kind:   rapidworkers.KindAITask,
		Status: rapidworkers.SubmissionStatusPending,
	})
	check(err)
	for _, s := range subs["submissions"].([]any) {
		sub := s.(map[string]any)
		_, err := rw.ApproveSubmission(ctx, rapidworkers.ApproveSubmissionParams{
			JobID:        jobID,
			Kind:         rapidworkers.KindAITask,
			SubmissionID: int(sub["submission_id"].(float64)),
		})
		check(err)
	}
}
```

Answers are the server's JSON as `map[string]any` (numbers are `float64`, as `encoding/json` decodes them). Field names follow the server; print a response to see its shape.

## Arguments

Each tool with arguments takes a `<Tool>Params` struct whose fields mirror the tool's JSON schema:

- **Required** arguments are plain fields (`PayRate float64`, `Confirm bool`).
- **Optional** arguments are pointers, so an unset one is left out of the call while `false` or `0` can still be sent. Set them with `rapidworkers.String`, `Int`, `Float`, `Bool` or the generic `Ptr`.
- **Enums** are named string types with constants (`rapidworkers.KindAITask`, `rapidworkers.JobStatusActive`); the empty value means unset. Optional lists are unset when nil.

```go
jobs, err := rw.ListJobs(ctx, rapidworkers.ListJobsParams{
	Status:   rapidworkers.JobStatusActive,
	PageSize: rapidworkers.Int(50),
})
```

## What the client does for you

- **One method per tool.** Every tool of the hosted [RapidWorkers MCP server](https://www.rapidworkers.io/mcp/) is a typed method on `*rapidworkers.Client`: account, platform rules, cost quotes, jobs, prefilled jobs, AI tasks, submissions and worker blocking. `rw.CallTool(ctx, name, map[string]any{...})` calls any tool by name, and `rw.ListTools(ctx)` returns the live definitions.
- **Money is never moved twice.** Calls that spend or pay out (`Create*`, `ExtendJob`, `CancelJob`, `Approve*`, `RejectSubmission`) get an idempotency key automatically, and a call that times out is retried with the same key. Set `IdempotencyKey` yourself to control this across processes.
- **Careful retries.** 429 and 503 answers are retried for every call; network errors, 502 and 504 only for calls that are safe to resend (read-only, idempotent, or with an idempotency key), so `GetDepositLink` is never sent twice. The wait doubles from 0.5s (at most 30s) or follows `Retry-After`. Configure with `WithMaxRetries` (default 2) and `WithTimeout` (per attempt, default 60s); every method also honours its `context.Context`.
- **Clear errors.** A refused call returns `*rapidworkers.ToolError` whose `Message` says how to fix it (for example a low balance or an expired `quote_id`) and whose `Code` is machine-readable. `*AuthenticationError`, `*RateLimitError`, `*ServiceUnavailableError`, `*APIError` and `*ConnectionError` cover the transport; the first three also match `*APIError` with `errors.As`.
- **Safe for concurrent use.** Share one `Client` across goroutines. `WithHTTPClient` and `WithBaseURL` customise the transport and endpoint (`RAPIDWORKERS_MCP_URL` also overrides it in `NewClientFromEnv`).

```go
_, err := rw.CreateJob(ctx, params)
var toolErr *rapidworkers.ToolError
if errors.As(err, &toolErr) {
	fmt.Println(toolErr.Code, toolErr.Message) // e.g. insufficient_balance
}
```

## Rules worth knowing

- At least $0.05 per worker ($0.10 for AI tasks, $0.50 per survey response) and at least 10 workers per job.
- A first deposit (minimum $20) is required before posting. `GetDepositLink` returns a checkout URL for a person to pay.
- Create calls need a `QuoteID` from `PreviewJobCost` and `Confirm: true`.
- Approving pays the worker and cannot be undone. Pending submissions are approved automatically after 2 days.
- AI tasks need access approved by the RapidWorkers team (`RequestAITaskAccess`).

## Use it from an AI agent

For Claude, Cursor and other MCP clients, connect the hosted server at `https://www.rapidworkers.io/api/mcp/` directly (OAuth sign-in), or run the [`rapidworkers-mcp`](cmd/rapidworkers-mcp) command with an API key:

```bash
go install github.com/RapidWorkers-io/rapidworkers/go/cmd/rapidworkers-mcp@latest
```

## Links

- Docs: <https://www.rapidworkers.io/developers/>
- API reference: <https://pkg.go.dev/github.com/RapidWorkers-io/rapidworkers/go>
- Source: <https://github.com/RapidWorkers-io/rapidworkers>
- License: MIT

`tools_gen.go` is generated from `spec/tools.json` by `python3 scripts/codegen.py` at the repository root; do not edit it by hand.
