package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rapidworkers "github.com/RapidWorkers-io/rapidworkers/go"
	"github.com/RapidWorkers-io/rapidworkers/go/internal/jsonrpc"
)

const (
	defaultProtocolVersion = "2025-06-18"
	// 429 and 503 are answered before anything ran upstream, so resending is
	// always safe.
	maxRetries = 2

	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
	internalError  = -32603
	upstreamError  = -32000
)

var knownProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var noKeyMessage = rapidworkers.APIKeyEnv + " is not set. Create an API key at " + rapidworkers.APIKeysURL +
	` and add it to this MCP server's environment (for example "env": {"` + rapidworkers.APIKeyEnv +
	`": "..."}), then restart the client.`

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func errorResponse(id json.RawMessage, code int64, message string, data json.RawMessage) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}

func resultResponse(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

type proxy struct {
	apiKey string
	url    string
	spec   *spec
	http   *http.Client
	log    *log.Logger
	// protocolVersion is the version the upstream initialize answer chose,
	// sent as MCP-Protocol-Version on later requests.
	protocolVersion atomic.Pointer[string]
	sleep           func(context.Context, time.Duration)
}

func newProxy(apiKey, url string, s *spec, logger *log.Logger) *proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &proxy{
		apiKey: apiKey,
		url:    url,
		spec:   s,
		http:   &http.Client{Timeout: 120 * time.Second, Transport: transport},
		log:    logger,
		sleep:  sleepContext,
	}
}

// serve reads newline-delimited JSON-RPC from in and writes each answer to
// out as one line. Requests are handled concurrently; it returns at EOF once
// every answer is written.
func serve(ctx context.Context, p *proxy, in io.Reader, out io.Writer) {
	reader := bufio.NewReaderSize(in, 1<<20) // ReadBytes also takes longer lines
	w := &lineWriter{out: out, log: p.log}
	var wg sync.WaitGroup
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.answer(ctx, line, w.write)
			}()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Printf("reading stdin: %v", err)
			}
			break
		}
	}
	wg.Wait()
}

type lineWriter struct {
	mu  sync.Mutex
	out io.Writer
	log *log.Logger
}

func (w *lineWriter) write(message any) {
	data, err := jsonrpc.Marshal(message)
	if err != nil {
		w.log.Printf("internal error: encoding an answer: %v", err)
		return
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.out.Write(data); err != nil {
		w.log.Printf("writing to stdout: %v", err)
	}
}

// answer handles one line: a message or a batch, whose answers are written
// one per line.
func (p *proxy) answer(ctx context.Context, line []byte, write func(any)) {
	line = bytes.TrimSpace(line)
	if !json.Valid(line) {
		write(errorResponse(nil, parseError, "Parse error.", nil))
		return
	}
	messages := []json.RawMessage{line}
	if line[0] == '[' {
		var batch []json.RawMessage
		_ = json.Unmarshal(line, &batch) // valid JSON, so this cannot fail
		messages = batch
	}
	for _, message := range messages {
		if reply := p.safeHandle(ctx, message); reply != nil {
			write(reply)
		}
	}
}

// safeHandle never lets one request kill the server.
func (p *proxy) safeHandle(ctx context.Context, message json.RawMessage) (reply any) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Printf("internal error: %v", r)
			var m struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(message, &m)
			reply = errorResponse(m.ID, internalError, "Internal error in rapidworkers-mcp.", nil)
		}
	}()
	return p.handle(ctx, message)
}

// handle returns the answer to one client message, or nil when none is due.
func (p *proxy) handle(ctx context.Context, raw json.RawMessage) any {
	var message jsonrpc.Message
	if json.Unmarshal(raw, &message) != nil || message == nil {
		return errorResponse(nil, invalidRequest, "Invalid JSON-RPC message.", nil)
	}
	if version, ok := jsonrpc.String(message["jsonrpc"]); !ok || version != "2.0" {
		return errorResponse(nil, invalidRequest, "Invalid JSON-RPC message.", nil)
	}
	method, ok := jsonrpc.String(message["method"])
	if !ok {
		return nil // a response to a server request; this server sends none
	}
	id, ok := message["id"]
	if !ok {
		return nil // notifications (initialized, cancelled...) need no answer
	}
	if method == "ping" {
		return resultResponse(id, struct{}{})
	}
	if p.apiKey == "" {
		return p.offline(id, method, message["params"])
	}
	return p.forward(ctx, raw, id, method)
}

// forward sends the message unchanged to the hosted server and returns its
// answer with the client's id.
func (p *proxy) forward(ctx context.Context, raw, id json.RawMessage, method string) any {
	var answer *upstreamAnswer
	for attempt := 0; ; {
		var err error
		answer, err = p.post(ctx, raw)
		if err != nil {
			note := ""
			if method == "tools/call" {
				note = " If this call moves money, retry it with the same idempotency_key."
			}
			return errorResponse(id, upstreamError, fmt.Sprintf("Could not reach RapidWorkers (%s).%s", describe(err), note), nil)
		}
		if (answer.status == http.StatusTooManyRequests || answer.status == http.StatusServiceUnavailable) && attempt < maxRetries {
			attempt++
			p.sleep(ctx, retryDelay(answer.header, attempt))
			continue
		}
		break
	}

	message := jsonrpc.Find(answer.header.Get("Content-Type"), answer.body, id)
	if answer.status >= 400 || message == nil {
		var upstream struct {
			Code    json.RawMessage `json:"code"`
			Message json.RawMessage `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if message != nil {
			_ = json.Unmarshal(message["error"], &upstream)
		}
		text, ok := jsonrpc.String(upstream.Message)
		if !ok || text == "" {
			text = fmt.Sprintf("RapidWorkers answered HTTP %d.", answer.status)
		}
		if answer.status == http.StatusUnauthorized {
			text += " Check " + rapidworkers.APIKeyEnv + " or create a new key at " + rapidworkers.APIKeysURL
		}
		code := int64(upstreamError)
		var upstreamCode float64
		if json.Unmarshal(upstream.Code, &upstreamCode) == nil && upstreamCode != 0 && upstreamCode == math.Trunc(upstreamCode) {
			code = int64(upstreamCode)
		}
		data := upstream.Data
		if string(bytes.TrimSpace(data)) == "null" {
			data = nil
		}
		return errorResponse(id, code, text, data)
	}
	if method == "initialize" {
		var result map[string]json.RawMessage
		if json.Unmarshal(message["result"], &result) == nil && result != nil {
			if version, ok := jsonrpc.String(result["protocolVersion"]); ok {
				p.protocolVersion.Store(&version)
			} else {
				p.protocolVersion.Store(nil)
			}
		}
	}
	message["id"] = id
	return message
}

type upstreamAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (p *proxy) post(ctx context.Context, body []byte) (*upstreamAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "rapidworkers-mcp-go/"+rapidworkers.Version)
	if version := p.protocolVersion.Load(); version != nil {
		req.Header.Set("MCP-Protocol-Version", *version)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &upstreamAnswer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// describe names a transport error for the client, without the URL.
func describe(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// retryDelay is the server's Retry-After (0 to 30s), or 0.5s * 2^attempt.
func retryDelay(header http.Header, attempt int) time.Duration {
	if s, err := strconv.ParseFloat(strings.TrimSpace(header.Get("Retry-After")), 64); err == nil && !math.IsNaN(s) {
		return seconds(min(max(s, 0), 30))
	}
	return seconds(min(0.5*math.Pow(2, float64(attempt)), 30))
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

func sleepContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

type serverInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions"`
}

// offline answers without an API key: enough to start and list the tools.
func (p *proxy) offline(id json.RawMessage, method string, rawParams json.RawMessage) any {
	switch method {
	case "initialize":
		var params struct {
			ProtocolVersion json.RawMessage `json:"protocolVersion"`
		}
		_ = json.Unmarshal(rawParams, &params)
		version := defaultProtocolVersion
		if requested, ok := jsonrpc.String(params.ProtocolVersion); ok && slices.Contains(knownProtocolVersions, requested) {
			version = requested
		}
		server := p.spec.Server
		return resultResponse(id, initializeResult{
			ProtocolVersion: version,
			Capabilities:    map[string]any{"tools": map[string]any{"listChanged": false}},
			ServerInfo:      serverInfo{Name: server.Name, Title: server.Title, Version: rapidworkers.Version},
			Instructions:    server.Instructions + "\n\n" + noKeyMessage,
		})
	case "tools/list":
		return resultResponse(id, map[string]json.RawMessage{"tools": p.spec.Tools})
	case "tools/call":
		return resultResponse(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": noKeyMessage}},
			"isError": true,
		})
	case "resources/list":
		return resultResponse(id, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return resultResponse(id, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return resultResponse(id, map[string]any{"prompts": []any{}})
	}
	return errorResponse(id, methodNotFound, method+" needs "+rapidworkers.APIKeyEnv+". "+noKeyMessage, nil)
}
