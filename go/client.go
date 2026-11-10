package rapidworkers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/RapidWorkers-io/rapidworkers/go/internal/jsonrpc"
)

const (
	// APIKeyEnv is the environment variable [NewClientFromEnv] reads the API
	// key from.
	APIKeyEnv = "RAPIDWORKERS_API_KEY"
	// BaseURLEnv is the environment variable that overrides the endpoint in
	// [NewClientFromEnv].
	BaseURLEnv = "RAPIDWORKERS_MCP_URL"
	// APIKeysURL is the dashboard page where API keys are created.
	APIKeysURL = "https://www.rapidworkers.io/dashboard/api-management/"
	// ProtocolVersion is the MCP protocol version the client speaks.
	ProtocolVersion = "2025-06-18"
	// DefaultTimeout bounds each HTTP attempt.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxRetries is how many times a failed call is resent by default.
	DefaultMaxRetries = 2

	maxBackoff        = 30 * time.Second
	idempotencyKeyArg = "idempotency_key"
)

// Client calls the hosted RapidWorkers MCP server. Every tool is a method;
// see the package documentation for an example.
//
// Each call is one JSON-RPC tools/call POST to the stateless endpoint, so the
// client gets exactly the validation, cost quotes, safeguards and error
// messages the server gives AI agents. A Client is safe for concurrent use.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
	maxRetries int
	ids        atomic.Int64
	sleep      func(context.Context, time.Duration) error
}

// Option configures a [Client].
type Option func(*Client)

// WithBaseURL sets the MCP endpoint. The default is [ServerURL].
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

// WithHTTPClient sets the HTTP client used for requests, for example to add
// a proxy or custom transport. The default is a new &http.Client{}.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// WithMaxRetries sets how many times a failed call is resent (default
// [DefaultMaxRetries]). 0 disables retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		c.maxRetries = max(0, n)
	}
}

// WithTimeout bounds each HTTP attempt (default [DefaultTimeout]); retries
// get a fresh timeout. 0 or less means no per-attempt limit: the context
// passed to each call still applies.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.timeout = max(0, d)
	}
}

// NewClient returns a client that authenticates with apiKey. Create a key at
// [APIKeysURL]. It returns [ErrNoAPIKey] when apiKey is empty.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, ErrNoAPIKey
	}
	c := &Client{
		apiKey:     apiKey,
		baseURL:    ServerURL,
		httpClient: &http.Client{},
		timeout:    DefaultTimeout,
		maxRetries: DefaultMaxRetries,
		sleep:      sleepContext,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if u, err := url.Parse(c.baseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("rapidworkers: invalid base URL %q", c.baseURL)
	}
	return c, nil
}

// NewClientFromEnv returns a client that reads its API key from
// RAPIDWORKERS_API_KEY. RAPIDWORKERS_MCP_URL, when set, overrides the
// endpoint; a [WithBaseURL] option overrides both.
func NewClientFromEnv(opts ...Option) (*Client, error) {
	if baseURL := strings.TrimSpace(os.Getenv(BaseURLEnv)); baseURL != "" {
		opts = append([]Option{WithBaseURL(baseURL)}, opts...)
	}
	return NewClient(os.Getenv(APIKeyEnv), opts...)
}

// CallTool calls any tool by name. args are the tool's arguments as its JSON
// schema names them; nil values are left out. When the tool takes an
// idempotency_key and args has none, a new one is created and reused on
// retries.
//
// A refused call returns a [*ToolError] whose Message says how to fix it.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	prepared := make(map[string]any, len(args)+1)
	for key, value := range args {
		if !isNil(value) {
			prepared[key] = value
		}
	}
	if info, ok := lookupTool(name); ok && info.idempotencyKey {
		if given, ok := prepared[idempotencyKeyArg]; !ok || given == "" {
			key, err := newIdempotencyKey()
			if err != nil {
				return nil, err
			}
			prepared[idempotencyKeyArg] = key
		}
	}
	return c.callTool(ctx, name, prepared)
}

// ListTools returns the live tool definitions (name, title, description,
// inputSchema, annotations).
func (c *Client) ListTools(ctx context.Context) ([]map[string]any, error) {
	result, err := c.send(ctx, "tools/list", struct{}{}, true)
	if err != nil {
		return nil, err
	}
	var list struct {
		Tools []map[string]any `json:"tools"`
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &list); err != nil {
			return nil, fmt.Errorf("rapidworkers: unreadable tools/list answer: %w", err)
		}
	}
	return list.Tools, nil
}

// toolInfo says how a tool may be retried and whether it takes an
// idempotency key. toolTable in tools_gen.go lists every tool.
type toolInfo struct {
	name           string
	readOnly       bool
	idempotencyKey bool
	// retry: safe to resend after a timeout, because nothing changes or the
	// server dedupes the call by its idempotency key.
	retry bool
}

func lookupTool(name string) (toolInfo, bool) {
	for _, info := range toolTable {
		if info.name == name {
			return info, true
		}
	}
	return toolInfo{}, false
}

type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c *Client) callTool(ctx context.Context, name string, args any) (map[string]any, error) {
	arguments, err := jsonrpc.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("rapidworkers: encoding the arguments of %s: %w", name, err)
	}
	info, _ := lookupTool(name)
	result, err := c.send(ctx, "tools/call", toolCall{Name: name, Arguments: arguments}, info.retry)
	if err != nil {
		return nil, err
	}
	return toolResult(name, result)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type httpAnswer struct {
	status int
	header http.Header
	body   []byte
}

// send posts one JSON-RPC request, retrying as the policy allows, and returns
// its result.
//
// 429 and 503 are answered before anything runs, so they are retried for every
// call. A 502, 504 or network error may come after the tool ran, so those are
// retried only when retrySafe is true.
func (c *Client) send(ctx context.Context, method string, params any, retrySafe bool) (json.RawMessage, error) {
	rawParams, err := jsonrpc.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("rapidworkers: encoding %s params: %w", method, err)
	}
	for attempt := 0; ; attempt++ {
		id := c.ids.Add(1)
		body, err := jsonrpc.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams})
		if err != nil {
			return nil, fmt.Errorf("rapidworkers: encoding %s request: %w", method, err)
		}
		answer, err := c.post(ctx, body)
		if err != nil {
			if ctx.Err() == nil && retrySafe && attempt < c.maxRetries {
				if err := c.sleep(ctx, backoff(attempt, nil)); err != nil {
					return nil, err
				}
				continue
			}
			return nil, &ConnectionError{Err: err}
		}
		if shouldRetry(answer.status, retrySafe) && attempt < c.maxRetries {
			if err := c.sleep(ctx, backoff(attempt, answer.header)); err != nil {
				return nil, err
			}
			continue
		}
		return unwrap(answer, id)
	}
}

func (c *Client) post(ctx context.Context, body []byte) (*httpAnswer, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	req.Header.Set("User-Agent", "rapidworkers-go/"+Version)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &httpAnswer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func shouldRetry(status int, retrySafe bool) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return retrySafe
	}
	return false
}

// backoff is 0.5s * 2^attempt, or the server's Retry-After, capped at 30s.
func backoff(attempt int, header http.Header) time.Duration {
	if hinted, ok := retryAfter(header); ok {
		return min(hinted, maxBackoff)
	}
	return min(seconds(0.5*math.Pow(2, float64(attempt))), maxBackoff)
}

func retryAfter(header http.Header) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	s, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(s) {
		return 0, false
	}
	return seconds(s), true
}

func seconds(s float64) time.Duration {
	switch {
	case s <= 0:
		return 0
	case s >= float64(math.MaxInt64)/float64(time.Second):
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s * float64(time.Second))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func decodeRPCError(raw json.RawMessage) rpcError {
	var e rpcError
	_ = json.Unmarshal(raw, &e) // keep whatever fields have the expected types
	return e
}

// unwrap turns an HTTP answer into the JSON-RPC result or an error.
func unwrap(answer *httpAnswer, id int64) (json.RawMessage, error) {
	message := jsonrpc.Find(answer.header.Get("Content-Type"), answer.body, json.RawMessage(strconv.FormatInt(id, 10)))
	if answer.status >= 400 {
		return nil, httpError(answer, message)
	}
	if message == nil {
		return nil, &APIError{StatusCode: answer.status, Message: "Unreadable answer from RapidWorkers."}
	}
	if raw, ok := message["error"]; ok {
		e := decodeRPCError(raw)
		if e.Message == "" {
			e.Message = "JSON-RPC error"
		}
		return nil, &APIError{StatusCode: answer.status, Code: e.Code, Message: e.Message, Data: e.Data}
	}
	return message["result"], nil
}

func httpError(answer *httpAnswer, message jsonrpc.Message) error {
	var e rpcError
	if message != nil {
		e = decodeRPCError(message["error"])
	}
	text := e.Message
	if text == "" {
		text = truncate(string(answer.body), 500)
	}
	if text == "" {
		text = fmt.Sprintf("HTTP %d", answer.status)
	}
	base := APIError{StatusCode: answer.status, Code: e.Code, Message: text, Data: e.Data}
	switch answer.status {
	case http.StatusUnauthorized:
		base.Message = text + " Check your API key or create one at " + APIKeysURL
		return &AuthenticationError{APIError: base}
	case http.StatusTooManyRequests:
		wait, _ := retryAfter(answer.header)
		return &RateLimitError{APIError: base, RetryAfter: wait}
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &ServiceUnavailableError{APIError: base}
	}
	return &base
}

func truncate(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	return string([]rune(s)[:runes])
}

// toolResult returns the structured content of a tools/call result, or a
// *ToolError when the tool refused the call.
func toolResult(tool string, raw json.RawMessage) (map[string]any, error) {
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result) // null or a non-object counts as {}

	var structured map[string]any
	hasStructured := json.Unmarshal(result["structuredContent"], &structured) == nil && structured != nil

	var texts []string
	var blocks []json.RawMessage
	_ = json.Unmarshal(result["content"], &blocks)
	for _, b := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(b, &block) // a non-string text counts as ""
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")

	if truthy(result["isError"]) {
		details := map[string]any{}
		if hasStructured {
			if e, ok := structured["error"].(map[string]any); ok {
				for k, v := range e {
					details[k] = v
				}
			}
		}
		message, _ := details["message"].(string)
		delete(details, "message")
		if message == "" {
			message = text
		}
		if message == "" {
			message = "The tool call failed."
		}
		code, _ := details["code"].(string)
		return nil, &ToolError{Tool: tool, Message: message, Code: code, Details: details}
	}
	if hasStructured {
		return structured, nil
	}
	// Text-only answers carry a one-line summary, a blank line and the JSON.
	payload := ""
	if _, after, found := strings.Cut(text, "\n\n"); found {
		payload = after
	}
	if payload == "" {
		payload = text
	}
	var data any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return map[string]any{"text": text}, nil
	}
	if m, ok := data.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{"data": data}, nil
}

// truthy is JSON truthiness as Python sees it: false, 0, "", [], {} and null
// are false.
func truthy(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return false
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// ensureIdempotencyKey sets *key to a new key when it is empty. The generated
// methods call it once per call, so every retry resends the same key.
func ensureIdempotencyKey(key *string) error {
	if *key != "" {
		return nil
	}
	k, err := newIdempotencyKey()
	if err != nil {
		return err
	}
	*key = k
	return nil
}

// newIdempotencyKey returns "sdk-" and 32 random hex characters.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rapidworkers: creating an idempotency key: %w", err)
	}
	return "sdk-" + hex.EncodeToString(b[:]), nil
}
