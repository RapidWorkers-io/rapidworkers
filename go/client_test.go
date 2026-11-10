package rapidworkers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

var testKey = strings.Repeat("k", 64)

const jobID = "00000000-0000-0000-0000-000000000000"

// --------------------------------------------------------------------------
// A fake MCP endpoint that answers each request with the next reply.

type call struct {
	ctx    context.Context // canceled when the client hangs up
	Header http.Header
	Body   []byte
	ID     json.RawMessage
	Method string
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
}

type reply func(w http.ResponseWriter, c *call)

type fakeServer struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []*call
	replies []reply
}

// newServer answers the n-th request with replies[n], repeating the last one.
func newServer(t *testing.T, replies ...reply) *fakeServer {
	t.Helper()
	s := &fakeServer{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := &call{ctx: r.Context(), Header: r.Header.Clone(), Body: body}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Errorf("request body is not JSON: %v: %s", err, body)
		}
		c.ID, c.Method = msg.ID, msg.Method
		_ = json.Unmarshal(msg.Params, &c.Params)
		s.mu.Lock()
		n := len(s.calls)
		s.calls = append(s.calls, c)
		next := s.replies[min(n, len(s.replies)-1)]
		s.mu.Unlock()
		next(w, c)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *fakeServer) Calls() []*call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*call(nil), s.calls...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ok answers with a JSON-RPC result carrying the request's id.
func ok(result any) reply {
	return func(w http.ResponseWriter, c *call) {
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": c.ID, "result": result})
	}
}

func status(code int, body string, headers ...string) reply {
	return func(w http.ResponseWriter, _ *call) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// structured is a tools/call result as the server sends it.
func structured(data map[string]any) map[string]any {
	text, _ := json.Marshal(data)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "summary\n\n" + string(text)}},
		"structuredContent": data,
	}
}

// failingTransport fails the first `failures` requests with a network error
// and records every request body it sees.
type failingTransport struct {
	mu       sync.Mutex
	failures int
	bodies   [][]byte
}

func (f *failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return nil, errors.New("connect timeout")
	}
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	return http.DefaultTransport.RoundTrip(clone)
}

type sleepLog struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (l *sleepLog) get() []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Duration(nil), l.sleeps...)
}

// newClient returns a client for url whose backoff only records the waits.
func newClient(t *testing.T, url string, opts ...Option) (*Client, *sleepLog) {
	t.Helper()
	c, err := NewClient(testKey, append([]Option{WithBaseURL(url)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	log := &sleepLog{}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		log.mu.Lock()
		log.sleeps = append(log.sleeps, d)
		log.mu.Unlock()
		return ctx.Err()
	}
	return c, log
}

func mustEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

var keyPattern = regexp.MustCompile(`^sdk-[0-9a-f]{32}$`)

// --------------------------------------------------------------------------
// Construction

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient("  "); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("NewClient(blank) = %v, want ErrNoAPIKey", err)
	}
	t.Setenv(APIKeyEnv, "")
	_, err := NewClientFromEnv()
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("NewClientFromEnv() = %v, want ErrNoAPIKey", err)
	}
	if !strings.Contains(err.Error(), "api-management") {
		t.Fatalf("error does not say where to create a key: %v", err)
	}
}

func TestNewClientFromEnv(t *testing.T) {
	t.Setenv(APIKeyEnv, " "+strings.Repeat("envkey", 5)+"\n")
	t.Setenv(BaseURLEnv, "")
	c, err := NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, c.apiKey, strings.Repeat("envkey", 5))
	mustEqual(t, c.baseURL, ServerURL)
	mustEqual(t, c.maxRetries, DefaultMaxRetries)
	mustEqual(t, c.timeout, DefaultTimeout)

	t.Setenv(BaseURLEnv, "https://mcp.test/api/mcp/")
	c, _ = NewClientFromEnv()
	mustEqual(t, c.baseURL, "https://mcp.test/api/mcp/")
	c, _ = NewClientFromEnv(WithBaseURL("http://127.0.0.1:1/mcp/"))
	mustEqual(t, c.baseURL, "http://127.0.0.1:1/mcp/")
}

func TestOptions(t *testing.T) {
	hc := &http.Client{}
	c, err := NewClient(testKey, WithHTTPClient(hc), WithMaxRetries(-1), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.httpClient != hc || c.maxRetries != 0 || c.timeout != 5*time.Second {
		t.Fatalf("options not applied: %+v", c)
	}
	if _, err := NewClient(testKey, WithBaseURL("not a url")); err == nil {
		t.Fatal("an invalid base URL was accepted")
	}
}

// --------------------------------------------------------------------------
// Requests

func TestToolCallSendsJSONRPCAndReturnsStructuredContent(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"balance": "12.50"})))
	c, _ := newClient(t, s.URL)
	got, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"balance": "12.50"})

	req := s.Calls()[0]
	for header, want := range map[string]string{
		"X-API-Key":            testKey,
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/event-stream",
		"MCP-Protocol-Version": "2025-06-18",
		"User-Agent":           "rapidworkers-go/" + Version,
	} {
		mustEqual(t, req.Header.Get(header), want)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, isNumber := body["id"].(float64); !isNumber {
		t.Fatalf("id is not a number: %#v", body["id"])
	}
	delete(body, "id")
	mustEqual(t, body, map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params":  map[string]any{"name": "get_account", "arguments": map[string]any{}},
	})
}

func TestUnsetArgumentsAreDropped(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"jobs": []any{}})))
	c, _ := newClient(t, s.URL)
	if _, err := c.ListJobs(context.Background(), ListJobsParams{Kind: KindAITask}); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, s.Calls()[0].Params.Arguments, map[string]any{"kind": "ai_task"})
}

func TestTypedArgumentsAreEncodedAsTheSchemaNamesThem(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"job_id": jobID})))
	c, _ := newClient(t, s.URL)
	_, err := c.CreateAITask(context.Background(), CreateAITaskParams{
		TaskType:               AITaskTypeCustomDataCollection,
		Title:                  "Cars",
		Description:            "Photograph a car.",
		SubmissionInstructions: "One photo.",
		PayRate:                0.25,
		Quantity:               10,
		LengthInMinutes:        3,
		Countries:              []string{"US"},
		QuoteID:                "quote-1234567890",
		Confirm:                true,
		SubmissionFields: []SubmissionField{{
			Type:     SubmissionFieldTypeSelect,
			Label:    "Body type",
			Required: Bool(false),
			Options:  []string{"Sedan", "SUV"},
		}},
		ReservationMinutes: Int(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	args := s.Calls()[0].Params.Arguments
	key, _ := args["idempotency_key"].(string)
	if !keyPattern.MatchString(key) {
		t.Fatalf("idempotency_key = %q, want sdk- and 32 hex characters", key)
	}
	delete(args, "idempotency_key")
	mustEqual(t, args, map[string]any{
		"task_type":               "custom_data_collection",
		"title":                   "Cars",
		"description":             "Photograph a car.",
		"submission_instructions": "One photo.",
		"pay_rate":                0.25,
		"quantity":                float64(10),
		"length_in_minutes":       float64(3),
		"countries":               []any{"US"},
		"quote_id":                "quote-1234567890",
		"confirm":                 true,
		"submission_fields": []any{map[string]any{
			"type": "select", "label": "Body type", "required": false, "options": []any{"Sedan", "SUV"},
		}},
		"reservation_minutes": float64(0), // a set pointer is sent even when zero
	})
}

func TestMoneyCallsGetAnIdempotencyKeyReusedOnRetry(t *testing.T) {
	// A network error, then a gateway timeout, then the answer.
	s := newServer(t, status(504, ""), ok(structured(map[string]any{"approved": true})))
	transport := &failingTransport{failures: 1}
	c, sleeps := newClient(t, s.URL, WithHTTPClient(&http.Client{Transport: transport}))

	got, err := c.ApproveSubmission(context.Background(), ApproveSubmissionParams{JobID: jobID, SubmissionID: 5})
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"approved": true})
	if len(transport.bodies) != 3 {
		t.Fatalf("sent %d requests, want 3", len(transport.bodies))
	}
	keys := map[string]bool{}
	ids := map[string]bool{}
	for _, body := range transport.bodies {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatal(err)
		}
		keys[msg.Params.Arguments["idempotency_key"].(string)] = true
		ids[string(msg.ID)] = true
	}
	if len(keys) != 1 {
		t.Fatalf("idempotency keys %v, want one key reused on every attempt", keys)
	}
	for key := range keys {
		if !keyPattern.MatchString(key) {
			t.Fatalf("idempotency_key = %q, want sdk- and 32 hex characters", key)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("JSON-RPC ids %v, want a new id per attempt", ids)
	}
	mustEqual(t, sleeps.get(), []time.Duration{500 * time.Millisecond, time.Second})
}

func TestEachCallGetsItsOwnIdempotencyKey(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"ok": true})))
	c, _ := newClient(t, s.URL)
	for i := 0; i < 2; i++ {
		if _, err := c.ExtendJob(context.Background(), ExtendJobParams{JobID: jobID, AdditionalQuantity: 1, Confirm: true}); err != nil {
			t.Fatal(err)
		}
	}
	calls := s.Calls()
	if calls[0].Params.Arguments["idempotency_key"] == calls[1].Params.Arguments["idempotency_key"] {
		t.Fatal("two separate calls shared an idempotency key")
	}
}

func TestExplicitIdempotencyKeyIsKept(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"ok": true})))
	c, _ := newClient(t, s.URL)
	if _, err := c.CancelJob(context.Background(), CancelJobParams{JobID: "x", Confirm: true, IdempotencyKey: "mine-1"}); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, s.Calls()[0].Params.Arguments["idempotency_key"], "mine-1")
}

func TestNonIdempotentWriteIsNotRetriedAfterGatewayError(t *testing.T) {
	s := newServer(t, status(504, "gateway timeout"))
	c, _ := newClient(t, s.URL)
	_, err := c.GetDepositLink(context.Background(), GetDepositLinkParams{Amount: 20})
	var unavailable *ServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("err = %v, want *ServiceUnavailableError", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 504 || apiErr.Message != "gateway timeout" {
		t.Fatalf("errors.As(*APIError) = %+v", apiErr)
	}
	mustEqual(t, len(s.Calls()), 1)
}

func TestNonIdempotentWriteIsNotRetriedAfterNetworkError(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"url": "https://pay"})))
	transport := &failingTransport{failures: 1}
	c, _ := newClient(t, s.URL, WithHTTPClient(&http.Client{Transport: transport}))
	_, err := c.GetDepositLink(context.Background(), GetDepositLinkParams{Amount: 20})
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || !strings.Contains(err.Error(), "connect timeout") {
		t.Fatalf("err = %v, want *ConnectionError", err)
	}
	mustEqual(t, len(transport.bodies), 1)
}

func Test429IsRetriedForAnyTool(t *testing.T) {
	s := newServer(t, status(429, "", "Retry-After", "1"), ok(structured(map[string]any{"url": "https://pay"})))
	c, sleeps := newClient(t, s.URL)
	got, err := c.GetDepositLink(context.Background(), GetDepositLinkParams{Amount: 20, Method: DepositMethodCard})
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"url": "https://pay"})
	mustEqual(t, len(s.Calls()), 2)
	mustEqual(t, sleeps.get(), []time.Duration{time.Second})
}

func Test503IsRetriedForAnyTool(t *testing.T) {
	s := newServer(t, status(503, ""), ok(structured(map[string]any{"url": "https://pay"})))
	c, _ := newClient(t, s.URL)
	if _, err := c.GetDepositLink(context.Background(), GetDepositLinkParams{Amount: 20}); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, len(s.Calls()), 2)
}

func TestRateLimitErrorAfterRetriesRunOut(t *testing.T) {
	s := newServer(t, status(429, `{"jsonrpc":"2.0","id":null,"error":{"code":-32029,"message":"Slow down."}}`, "Retry-After", "7"))
	c, sleeps := newClient(t, s.URL)
	_, err := c.GetAccount(context.Background())
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	mustEqual(t, limited.RetryAfter, 7*time.Second)
	mustEqual(t, limited.Message, "Slow down.")
	mustEqual(t, limited.Code, -32029)
	mustEqual(t, len(s.Calls()), 3) // the first try and DefaultMaxRetries retries
	mustEqual(t, sleeps.get(), []time.Duration{7 * time.Second, 7 * time.Second})
}

func TestWithMaxRetriesZeroDisablesRetries(t *testing.T) {
	s := newServer(t, status(503, ""))
	c, _ := newClient(t, s.URL, WithMaxRetries(0))
	if _, err := c.GetAccount(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	mustEqual(t, len(s.Calls()), 1)
}

func TestToolErrorRaisesWithDetails(t *testing.T) {
	s := newServer(t, ok(map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "Balance too low."}},
		"structuredContent": map[string]any{"error": map[string]any{"message": "Balance too low.", "code": "insufficient_balance"}},
		"isError":           true,
	}))
	c, _ := newClient(t, s.URL)
	_, err := c.PreviewJobCost(context.Background(), PreviewJobCostParams{PayRate: 0.05, Quantity: 10})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %v, want *ToolError", err)
	}
	mustEqual(t, toolErr.Message, "Balance too low.")
	mustEqual(t, toolErr.Code, "insufficient_balance")
	mustEqual(t, toolErr.Tool, "preview_job_cost")
	mustEqual(t, toolErr.Details, map[string]any{"code": "insufficient_balance"})
	mustEqual(t, err.Error(), "rapidworkers: preview_job_cost: Balance too low.")
}

func TestToolErrorWithTextOnly(t *testing.T) {
	s := newServer(t, ok(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "quote_id expired."}},
		"isError": true,
	}))
	c, _ := newClient(t, s.URL)
	_, err := c.GetJob(context.Background(), GetJobParams{JobID: jobID})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Message != "quote_id expired." || toolErr.Code != "" {
		t.Fatalf("err = %#v", err)
	}
}

func Test401RaisesAuthenticationError(t *testing.T) {
	s := newServer(t, status(401, `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Invalid token."}}`,
		"Content-Type", "application/json"))
	c, _ := newClient(t, s.URL)
	_, err := c.GetAccount(context.Background())
	var authErr *AuthenticationError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want *AuthenticationError", err)
	}
	if !strings.HasPrefix(authErr.Message, "Invalid token. ") || !strings.HasSuffix(authErr.Message, "create one at "+APIKeysURL) {
		t.Fatalf("message = %q", authErr.Message)
	}
	mustEqual(t, authErr.StatusCode, 401)
	mustEqual(t, authErr.Code, -32001)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatal("an AuthenticationError must also match *APIError")
	}
	mustEqual(t, len(s.Calls()), 1)
}

func TestOtherHTTPErrorsAreAPIErrors(t *testing.T) {
	s := newServer(t, status(400, "bad request body"))
	c, _ := newClient(t, s.URL)
	_, err := c.GetAccount(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	mustEqual(t, *apiErr, APIError{StatusCode: 400, Message: "bad request body"})
	mustEqual(t, err.Error(), "rapidworkers: bad request body (HTTP 400)")

	s = newServer(t, status(500, ""))
	c, _ = newClient(t, s.URL)
	_, err = c.GetAccount(context.Background())
	if !errors.As(err, &apiErr) || apiErr.Message != "HTTP 500" {
		t.Fatalf("err = %v", err)
	}
}

func TestJSONRPCErrorAnswer(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, c *call) {
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": c.ID, "error": map[string]any{
			"code": -32602, "message": "Unknown tool: nope", "data": map[string]any{"tool": "nope"},
		}})
	})
	c, _ := newClient(t, s.URL)
	_, err := c.CallTool(context.Background(), "nope", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	mustEqual(t, *apiErr, APIError{StatusCode: 200, Code: -32602, Message: "Unknown tool: nope", Data: map[string]any{"tool": "nope"}})
}

func TestUnreadableAnswer(t *testing.T) {
	s := newServer(t, status(200, "<html>oops</html>"))
	c, _ := newClient(t, s.URL)
	_, err := c.GetAccount(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "Unreadable answer from RapidWorkers." {
		t.Fatalf("err = %v", err)
	}
}

func TestSSEAnswerIsParsed(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, c *call) {
		answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.ID, "result": structured(map[string]any{"n": 1})})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		fmt.Fprintf(w, "event: message\r\ndata: %s\r\n\r\n", answer)
	})
	c, _ := newClient(t, s.URL)
	got, err := c.ListBlockedWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"n": float64(1)})
}

func TestBatchAnswerIsMatchedByID(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, c *call) {
		writeJSON(w, 200, []any{
			map[string]any{"jsonrpc": "2.0", "id": 999999, "result": structured(map[string]any{"wrong": true})},
			map[string]any{"jsonrpc": "2.0", "id": c.ID, "result": structured(map[string]any{"right": true})},
		})
	})
	c, _ := newClient(t, s.URL)
	got, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"right": true})
}

func TestTextOnlyResultIsParsed(t *testing.T) {
	text := func(s string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
	}
	for _, tc := range []struct {
		text string
		want map[string]any
	}{
		{"One job.\n\n{\"jobs\": [1]}", map[string]any{"jobs": []any{float64(1)}}},
		{"{\"jobs\": []}", map[string]any{"jobs": []any{}}},
		{"Two items.\n\n[1, 2]", map[string]any{"data": []any{float64(1), float64(2)}}},
		{"Done, nothing else.", map[string]any{"text": "Done, nothing else."}},
	} {
		s := newServer(t, ok(text(tc.text)))
		c, _ := newClient(t, s.URL)
		got, err := c.ListJobs(context.Background(), ListJobsParams{})
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, got, tc.want)
	}
}

func TestListTools(t *testing.T) {
	s := newServer(t, ok(map[string]any{"tools": []any{map[string]any{"name": "get_account"}}}))
	c, _ := newClient(t, s.URL)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, tools, []map[string]any{{"name": "get_account"}})
	mustEqual(t, s.Calls()[0].Method, "tools/list")
}

func TestCallToolPreparesArguments(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"ok": true})))
	c, _ := newClient(t, s.URL)
	var unset *string
	args := map[string]any{"job_id": jobID, "confirm": true, "kind": nil, "notes": unset}
	if _, err := c.CallTool(context.Background(), "approve_all_pending", args); err != nil {
		t.Fatal(err)
	}
	sent := s.Calls()[0].Params.Arguments
	key, _ := sent["idempotency_key"].(string)
	if !keyPattern.MatchString(key) {
		t.Fatalf("idempotency_key = %q", key)
	}
	delete(sent, "idempotency_key")
	mustEqual(t, sent, map[string]any{"job_id": jobID, "confirm": true})
	if _, changed := args["idempotency_key"]; changed {
		t.Fatal("CallTool changed the caller's map")
	}

	// Tools without an idempotency key get none.
	if _, err := c.CallTool(context.Background(), "get_account", nil); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, s.Calls()[1].Params.Arguments, map[string]any{})
}

func TestUnknownToolIsNotRetriedAfterNetworkError(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{})))
	transport := &failingTransport{failures: 1}
	c, _ := newClient(t, s.URL, WithHTTPClient(&http.Client{Transport: transport}))
	var connErr *ConnectionError
	if _, err := c.CallTool(context.Background(), "future_tool", nil); !errors.As(err, &connErr) {
		t.Fatalf("err = %v, want *ConnectionError", err)
	}
	mustEqual(t, len(transport.bodies), 1)
}

func TestCanceledContextStopsAtOnce(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{})))
	c, sleeps := newClient(t, s.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.GetAccount(ctx)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a *ConnectionError matching context.Canceled", err)
	}
	mustEqual(t, len(s.Calls()), 0)
	mustEqual(t, len(sleeps.get()), 0)
}

func TestCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newServer(t, func(w http.ResponseWriter, _ *call) {
		cancel()
		w.WriteHeader(503)
	})
	c, err := NewClient(testKey, WithBaseURL(s.URL)) // the real, context-aware sleep
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.GetAccount(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the backoff did not stop when the context was canceled")
	}
	mustEqual(t, len(s.Calls()), 1)
}

func TestTimeoutPerAttemptIsRetried(t *testing.T) {
	s := newServer(t,
		func(w http.ResponseWriter, c *call) {
			select { // slower than the timeout; the client gives up first
			case <-time.After(5 * time.Second):
			case <-c.ctx.Done():
			}
		},
		ok(structured(map[string]any{"balance": "1.00"})),
	)
	c, sleeps := newClient(t, s.URL, WithTimeout(100*time.Millisecond))
	got, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, got, map[string]any{"balance": "1.00"})
	mustEqual(t, len(s.Calls()), 2)
	mustEqual(t, len(sleeps.get()), 1)
}

func TestConcurrentCalls(t *testing.T) {
	s := newServer(t, ok(structured(map[string]any{"ok": true})))
	c, _ := newClient(t, s.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetAccount(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ids := map[string]bool{}
	for _, call := range s.Calls() {
		ids[string(call.ID)] = true
	}
	mustEqual(t, len(ids), 20)
}

func TestBackoff(t *testing.T) {
	header := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for _, tc := range []struct {
		attempt int
		header  http.Header
		want    time.Duration
	}{
		{0, nil, 500 * time.Millisecond},
		{1, nil, time.Second},
		{2, nil, 2 * time.Second},
		{6, nil, 30 * time.Second},
		{100, nil, 30 * time.Second},
		{0, header("2.5"), 2500 * time.Millisecond},
		{0, header("0"), 0},
		{0, header("-3"), 0},
		{0, header("100"), 30 * time.Second},
		{0, header("1e30"), 30 * time.Second},
		{1, header("soon"), time.Second},
		{1, header("NaN"), time.Second},
	} {
		if got := backoff(tc.attempt, tc.header); got != tc.want {
			t.Errorf("backoff(%d, %v) = %v, want %v", tc.attempt, tc.header, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// Generated code

type specFile struct {
	Server struct {
		URL string `json:"url"`
	} `json:"server"`
	Tools []struct {
		Name        string `json:"name"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"inputSchema"`
		Annotations struct {
			ReadOnlyHint   bool `json:"readOnlyHint"`
			IdempotentHint bool `json:"idempotentHint"`
		} `json:"annotations"`
	} `json:"tools"`
}

// loadSpec reads the copy of spec/tools.json that the relay embeds; it is part
// of this module and codegen.py --check keeps it identical to the spec.
func loadSpec(t *testing.T) specFile {
	t.Helper()
	data, err := os.ReadFile("cmd/rapidworkers-mcp/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec specFile
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

var initialisms = map[string]string{"ai": "AI", "api": "API", "id": "ID", "url": "URL", "uri": "URI", "json": "JSON", "http": "HTTP"}

func goName(s string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if up, ok := initialisms[part]; ok {
			b.WriteString(up)
		} else {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

func TestEverySpecToolHasAMethod(t *testing.T) {
	spec := loadSpec(t)
	if len(spec.Tools) == 0 {
		t.Fatal("no tools in the spec")
	}
	mustEqual(t, ServerURL, spec.Server.URL)
	mustEqual(t, len(toolTable), len(spec.Tools))
	client := reflect.TypeOf(&Client{})
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	resultType := reflect.TypeOf(map[string]any{})
	errType := reflect.TypeOf((*error)(nil)).Elem()
	for _, tool := range spec.Tools {
		name := goName(tool.Name)
		method, found := client.MethodByName(name)
		if !found {
			t.Errorf("tool %s has no method Client.%s", tool.Name, name)
			continue
		}
		mt := method.Type // the receiver is In(0)
		if mt.NumOut() != 2 || mt.Out(0) != resultType || mt.Out(1) != errType {
			t.Errorf("Client.%s returns %v, want (map[string]any, error)", name, mt)
		}
		if mt.NumIn() < 2 || mt.In(1) != ctxType {
			t.Errorf("Client.%s does not take a context first: %v", name, mt)
			continue
		}
		props := tool.InputSchema.Properties
		if len(props) == 0 {
			if mt.NumIn() != 2 {
				t.Errorf("Client.%s takes arguments, but the tool has none", name)
			}
		} else if mt.NumIn() != 3 || mt.In(2).Name() != name+"Params" {
			t.Errorf("Client.%s does not take a %sParams: %v", name, name, mt)
		} else {
			checkParams(t, tool.Name, mt.In(2), props, tool.InputSchema.Required)
		}

		info, ok := lookupTool(tool.Name)
		_, hasKey := props["idempotency_key"]
		a := tool.Annotations
		want := toolInfo{name: tool.Name, readOnly: a.ReadOnlyHint, idempotencyKey: hasKey, retry: a.ReadOnlyHint || a.IdempotentHint || hasKey}
		if !ok || info != want {
			t.Errorf("toolTable[%s] = %+v, want %+v", tool.Name, info, want)
		}
	}
}

// checkParams: one JSON field per schema property; required ones always sent,
// optional ones left out when unset.
func checkParams(t *testing.T, tool string, params reflect.Type, props map[string]json.RawMessage, required []string) {
	t.Helper()
	isRequired := map[string]bool{}
	for _, name := range required {
		isRequired[name] = name != "idempotency_key"
	}
	seen := map[string]bool{}
	for i := 0; i < params.NumField(); i++ {
		field := params.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")
		name, omitempty := tag[0], len(tag) > 1 && tag[1] == "omitempty"
		seen[name] = true
		if _, ok := props[name]; !ok {
			t.Errorf("%s: field %s (%q) is not in the schema", tool, field.Name, name)
		}
		if field.Name != goName(name) {
			t.Errorf("%s: field for %q is named %s", tool, name, field.Name)
		}
		if isRequired[name] == omitempty {
			t.Errorf("%s.%s: required=%v but omitempty=%v", tool, name, isRequired[name], omitempty)
		}
		kind := field.Type.Kind()
		if !isRequired[name] && name != "idempotency_key" && kind != reflect.Pointer && kind != reflect.Slice &&
			kind != reflect.Map && kind != reflect.Interface && field.Type.PkgPath() == "" {
			t.Errorf("%s.%s: optional %v cannot tell unset from zero", tool, name, field.Type)
		}
	}
	for name := range props {
		if !seen[name] {
			t.Errorf("%s: no field for %q", tool, name)
		}
	}
}

func TestGeneratedCodeIsGofmtClean(t *testing.T) {
	src, err := os.ReadFile("tools_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src, formatted) {
		t.Fatal("tools_gen.go is not gofmt-formatted; fix scripts/gen_go.py")
	}
}
