// Package rapidworkers is the official Go client for RapidWorkers: hire real
// people for microtasks, app testing, surveys and AI data collection (photos,
// audio, video, text). People do the work on their own devices; you pay in USD
// from your account balance and approve only the work that meets your brief.
//
// Create an API key at https://www.rapidworkers.io/dashboard/api-management/
// and set it as RAPIDWORKERS_API_KEY.
//
//	rw, err := rapidworkers.NewClientFromEnv()
//	if err != nil {
//		log.Fatal(err)
//	}
//	account, err := rw.GetAccount(ctx) // balance, deposit status, AI task access
//
// # Tools
//
// Every tool of the hosted RapidWorkers MCP server is a method on [Client]
// that takes a context and, when the tool has arguments, a Params struct:
// account, platform rules, cost quotes, jobs, prefilled jobs, AI tasks,
// submissions and worker blocking. Answers are the server's JSON as
// map[string]any; print one to see its shape. [Client.CallTool] calls any tool
// by name and [Client.ListTools] returns the live definitions.
//
// Required arguments are plain fields. Optional ones are pointers, set with
// [String], [Int], [Float], [Bool] or [Ptr], except enums and lists, which are
// unset at their zero value:
//
//	jobs, err := rw.ListJobs(ctx, rapidworkers.ListJobsParams{
//		Kind:     rapidworkers.KindAITask,
//		PageSize: rapidworkers.Int(50),
//	})
//
// # Money is never moved twice
//
// Calls that spend or pay out (CreateJob, CreatePrefilledJob, CreateAITask,
// ExtendJob, CancelJob, ApproveSubmission, RejectSubmission and
// ApproveAllPending) get an idempotency key automatically when
// Params.IdempotencyKey is empty, and a call that times out is resent with the
// same key. Set IdempotencyKey yourself to retry safely across processes.
//
// 429 and 503 answers are retried for every call. Network errors, 502 and 504
// are retried only for calls that are safe to resend: read-only tools,
// idempotent tools and tools with an idempotency key. GetDepositLink is not
// resent. The wait is 0.5s, 1s, 2s... (at most 30s) or the server's
// Retry-After; see [WithMaxRetries] and [WithTimeout].
//
// # Errors
//
// A refused call returns a [*ToolError] whose Message says how to fix it (for
// example a low balance or an expired quote_id). [*AuthenticationError],
// [*RateLimitError], [*ServiceUnavailableError] and [*APIError] cover HTTP and
// JSON-RPC errors, and [*ConnectionError] covers the network. Use errors.As:
//
//	var toolErr *rapidworkers.ToolError
//	if errors.As(err, &toolErr) {
//		fmt.Println(toolErr.Code, toolErr.Message)
//	}
//
// # Rules worth knowing
//
// Pay at least $0.05 per worker ($0.10 for AI tasks, $0.50 per survey response) and at least 10 workers
// per job. A first deposit (minimum $20) is required before posting;
// GetDepositLink returns a checkout URL for a person to pay. Create calls need
// a quote_id from PreviewJobCost and Confirm: true. Approving pays the worker
// and cannot be undone; pending submissions are approved automatically after
// 2 days. AI tasks need access approved by the RapidWorkers team
// (RequestAITaskAccess).
package rapidworkers
