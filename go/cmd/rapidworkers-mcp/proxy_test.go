package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	rapidworkers "github.com/RapidWorkers-io/rapidworkers/go"
)

var testKey = strings.Repeat("k", 64)

// upstream is a fake hosted MCP server.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func newUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r)
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) last() (*http.Request, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1], u.bodies[len(u.bodies)-1]
}

// echoResult answers with result and the request's own id.
func echoResult(result any) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, body []byte) {
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &msg)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
}

func testSpec(t *testing.T) *spec {
	t.Helper()
	s, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestProxy(t *testing.T, apiKey, url string) (*proxy, *[]time.Duration) {
	t.Helper()
	p := newProxy(apiKey, url, testSpec(t), log.New(io.Discard, "", 0))
	var mu sync.Mutex
	sleeps := &[]time.Duration{}
	p.sleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		*sleeps = append(*sleeps, d)
		mu.Unlock()
	}
	return p, sleeps
}

func rpc(method string, id any, params any) json.RawMessage {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	return data
}

// roundTrip encodes an answer as it would be written to stdout and decodes it.
func roundTrip(t *testing.T, answer any) map[string]any {
	t.Helper()
	if answer == nil {
		return nil
	}
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func mustEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func errorOf(t *testing.T, answer map[string]any) (code float64, message string) {
	t.Helper()
	e, ok := answer["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an error answer: %v", answer)
	}
	return e["code"].(float64), e["message"].(string)
}

// --------------------------------------------------------------------------
// Forwarding

func TestForwardsRequestUnchangedWithAPIKey(t *testing.T) {
	u := newUpstream(t, echoResult(map[string]any{"ok": 1}))
	p, _ := newTestProxy(t, testKey, u.URL)
	message := []byte(`{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": {"name": "get_account", "arguments": {}}}`)
	got := roundTrip(t, p.handle(context.Background(), message))
	mustEqual(t, got, map[string]any{"jsonrpc": "2.0", "id": float64(7), "result": map[string]any{"ok": float64(1)}})

	req, body := u.last()
	if !bytes.Equal(body, message) {
		t.Fatalf("forwarded body changed:\n%s\n%s", body, message)
	}
	mustEqual(t, req.Header.Get("X-API-Key"), testKey)
	mustEqual(t, req.Header.Get("Content-Type"), "application/json")
	mustEqual(t, req.Header.Get("Accept"), "application/json, text/event-stream")
	mustEqual(t, req.Header.Get("User-Agent"), "rapidworkers-mcp-go/"+rapidworkers.Version)
	mustEqual(t, req.Header.Get("MCP-Protocol-Version"), "") // not before initialize
}

func TestInitializeSetsProtocolHeaderForLaterCalls(t *testing.T) {
	u := newUpstream(t, echoResult(map[string]any{"protocolVersion": "2025-06-18"}))
	p, _ := newTestProxy(t, testKey, u.URL)
	p.handle(context.Background(), rpc("initialize", 1, map[string]any{"protocolVersion": "2025-06-18"}))
	p.handle(context.Background(), rpc("tools/list", 2, nil))
	req, _ := u.last()
	mustEqual(t, req.Header.Get("MCP-Protocol-Version"), "2025-06-18")
}

func TestSSEAnswer(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &msg)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", msg.ID)
	})
	p, _ := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/list", "abc", nil)))
	mustEqual(t, got["id"], "abc")
	mustEqual(t, got["result"], map[string]any{"tools": []any{}})
}

func TestAnswerGetsTheClientsID(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"other","result":{"x":true}}`)
	})
	p, _ := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/list", 12, nil)))
	mustEqual(t, got["id"], float64(12))
}

func Test401BecomesJSONRPCErrorWithKeyHint(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Invalid token."}}`)
	})
	p, _ := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/list", 3, nil)))
	mustEqual(t, got["id"], float64(3))
	code, message := errorOf(t, got)
	mustEqual(t, code, float64(-32001))
	if !strings.HasPrefix(message, "Invalid token. Check RAPIDWORKERS_API_KEY") || !strings.Contains(message, "api-management") {
		t.Fatalf("message = %q", message)
	}
}

func TestHTTPErrorWithoutJSONRPCBody(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "<html>oops</html>")
	})
	p, _ := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/list", 4, nil)))
	code, message := errorOf(t, got)
	mustEqual(t, code, float64(-32000))
	mustEqual(t, message, "RapidWorkers answered HTTP 500.")
	if _, hasData := got["error"].(map[string]any)["data"]; hasData {
		t.Fatal("unexpected error data")
	}
}

func Test503IsRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		echoResult(map[string]any{})(w, r, body)
	})
	p, sleeps := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/list", 4, nil)))
	mustEqual(t, got["result"], map[string]any{})
	mustEqual(t, u.count(), 2)
	mustEqual(t, *sleeps, []time.Duration{time.Second}) // 0.5s * 2^1
}

func Test429IsRetriedTwiceThenReported(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
	})
	p, sleeps := newTestProxy(t, testKey, u.URL)
	got := roundTrip(t, p.handle(context.Background(), rpc("tools/call", 5, map[string]any{"name": "get_account"})))
	code, message := errorOf(t, got)
	mustEqual(t, code, float64(-32000))
	mustEqual(t, message, "RapidWorkers answered HTTP 429.")
	mustEqual(t, u.count(), 3)
	mustEqual(t, *sleeps, []time.Duration{3 * time.Second, 3 * time.Second})
}

func TestNetworkErrorOnToolCallMentionsIdempotency(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // nothing listens on this address any more
	p, sleeps := newTestProxy(t, testKey, closed.URL)

	got := roundTrip(t, p.handle(context.Background(), rpc("tools/call", 5, map[string]any{"name": "approve_submission", "arguments": map[string]any{}})))
	code, message := errorOf(t, got)
	mustEqual(t, code, float64(-32000))
	if !strings.HasPrefix(message, "Could not reach RapidWorkers (") || !strings.Contains(message, "same idempotency_key") {
		t.Fatalf("message = %q", message)
	}
	mustEqual(t, len(*sleeps), 0) // network errors are not retried

	got = roundTrip(t, p.handle(context.Background(), rpc("tools/list", 6, nil)))
	if _, message := errorOf(t, got); strings.Contains(message, "idempotency_key") {
		t.Fatalf("tools/list error mentions idempotency: %q", message)
	}
}

func TestNotificationsPingAndResponsesAreHandledLocally(t *testing.T) {
	u := newUpstream(t, func(http.ResponseWriter, *http.Request, []byte) { t.Error("nothing should be forwarded") })
	p, _ := newTestProxy(t, testKey, u.URL)
	ctx := context.Background()
	if got := p.handle(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Fatalf("notification answered: %v", got)
	}
	if got := p.handle(ctx, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); got != nil {
		t.Fatalf("client response answered: %v", got)
	}
	mustEqual(t, roundTrip(t, p.handle(ctx, rpc("ping", 9, nil))), map[string]any{"jsonrpc": "2.0", "id": float64(9), "result": map[string]any{}})
}

func TestInvalidMessages(t *testing.T) {
	p, _ := newTestProxy(t, "", "http://127.0.0.1:9/")
	for _, raw := range []string{`1`, `null`, `"x"`, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, `{"id":1,"method":"ping"}`} {
		got := roundTrip(t, p.handle(context.Background(), []byte(raw)))
		code, _ := errorOf(t, got)
		if code != -32600 || got["id"] != nil {
			t.Errorf("%s: answer %v, want -32600 with a null id", raw, got)
		}
	}
}

// --------------------------------------------------------------------------
// Without an API key

func TestOfflineAnswers(t *testing.T) {
	p, _ := newTestProxy(t, "", "http://127.0.0.1:9/")
	ctx := context.Background()
	s := testSpec(t)

	init := roundTrip(t, p.handle(ctx, rpc("initialize", 1, map[string]any{"protocolVersion": "2025-03-26"})))
	result := init["result"].(map[string]any)
	mustEqual(t, result["protocolVersion"], "2025-03-26")
	mustEqual(t, result["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}})
	mustEqual(t, result["serverInfo"], map[string]any{"name": s.Server.Name, "title": s.Server.Title, "version": rapidworkers.Version})
	mustEqual(t, result["instructions"], s.Server.Instructions+"\n\n"+noKeyMessage)

	init = roundTrip(t, p.handle(ctx, rpc("initialize", 2, map[string]any{"protocolVersion": "1999-01-01"})))
	mustEqual(t, init["result"].(map[string]any)["protocolVersion"], "2025-06-18")

	var want []any
	if err := json.Unmarshal(s.Tools, &want); err != nil {
		t.Fatal(err)
	}
	tools := roundTrip(t, p.handle(ctx, rpc("tools/list", 3, nil)))["result"].(map[string]any)["tools"]
	mustEqual(t, tools, want)

	call := roundTrip(t, p.handle(ctx, rpc("tools/call", 4, map[string]any{"name": "get_account"})))["result"].(map[string]any)
	mustEqual(t, call["isError"], true)
	mustEqual(t, call["content"], []any{map[string]any{"type": "text", "text": noKeyMessage}})

	for method, key := range map[string]string{"resources/list": "resources", "resources/templates/list": "resourceTemplates", "prompts/list": "prompts"} {
		got := roundTrip(t, p.handle(ctx, rpc(method, 5, nil)))
		mustEqual(t, got["result"], map[string]any{key: []any{}})
	}

	got := roundTrip(t, p.handle(ctx, rpc("completion/complete", 6, nil)))
	code, message := errorOf(t, got)
	mustEqual(t, code, float64(-32601))
	if !strings.HasPrefix(message, "completion/complete needs RAPIDWORKERS_API_KEY. ") {
		t.Fatalf("message = %q", message)
	}
}

func TestBundledSpec(t *testing.T) {
	s := testSpec(t)
	var tools []map[string]any
	if err := json.Unmarshal(s.Tools, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 22 {
		t.Fatalf("%d tools in the bundled spec, want 22", len(tools))
	}
	mustEqual(t, s.Server.URL, rapidworkers.ServerURL)
	mustEqual(t, s.Server.Name, "rapidworkers")
}

// --------------------------------------------------------------------------
// The stdio loop

// readAnswers decodes every output line and indexes it by id.
func readAnswers(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	answers := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			t.Fatalf("output line is not JSON: %q", line)
		}
		answers[fmt.Sprint(answer["id"])] = answer
	}
	return answers
}

func TestServeParseErrorsBatchesAndBlankLines(t *testing.T) {
	p, _ := newTestProxy(t, "", "http://127.0.0.1:9/")
	in := strings.Join([]string{
		"not json",
		"",
		"   ",
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"prompts/list"}]`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`, // the last line has no newline
	}, "\n")
	var out bytes.Buffer
	serve(context.Background(), p, strings.NewReader(in), &out)
	answers := readAnswers(t, out.String())
	if len(answers) != 4 {
		t.Fatalf("answers: %v", answers)
	}
	code, _ := errorOf(t, answers["<nil>"])
	mustEqual(t, code, float64(-32700))
	mustEqual(t, answers["1"]["result"], map[string]any{})
	mustEqual(t, answers["2"]["result"], map[string]any{"prompts": []any{}})
	mustEqual(t, answers["3"]["result"], map[string]any{})
}

func TestServeTakesLongLines(t *testing.T) {
	p, _ := newTestProxy(t, "", "http://127.0.0.1:9/")
	in := string(rpc("tools/call", 1, map[string]any{"name": "create_job", "arguments": map[string]any{"description": strings.Repeat("x", 3<<20)}}))
	var out bytes.Buffer
	serve(context.Background(), p, strings.NewReader(in+"\n"), &out)
	answers := readAnswers(t, out.String())
	mustEqual(t, answers["1"]["result"].(map[string]any)["isError"], true)
}

func TestServeHandlesRequestsConcurrently(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		echoResult(map[string]any{"ok": true})(w, r, body)
	})
	p, _ := newTestProxy(t, testKey, u.URL)
	go func() {
		// Both requests must be upstream at the same time before either is answered.
		for i := 0; i < 2; i++ {
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Error("requests were not forwarded concurrently")
			}
		}
		close(release)
	}()
	in := string(rpc("tools/call", 1, map[string]any{"name": "list_jobs"})) + "\n" + string(rpc("tools/call", 2, map[string]any{"name": "get_account"})) + "\n"
	var out bytes.Buffer
	serve(context.Background(), p, strings.NewReader(in), &out)
	answers := readAnswers(t, out.String())
	mustEqual(t, answers["1"]["result"], map[string]any{"ok": true})
	mustEqual(t, answers["2"]["result"], map[string]any{"ok": true})
}

func TestRunForwardsOverStdio(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "initialize" {
			echoResult(map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]any{"name": "rapidworkers"}})(w, r, body)
			return
		}
		echoResult(map[string]any{"tools": []any{map[string]any{"name": "get_account"}}})(w, r, body)
	})
	env := map[string]string{rapidworkers.APIKeyEnv: testKey, rapidworkers.BaseURLEnv: u.URL}
	in := strings.Join([]string{
		string(rpc("initialize", 1, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}, "\n") + "\n"
	var out, errOut bytes.Buffer
	if code := run(nil, strings.NewReader(in), &out, &errOut, func(k string) string { return env[k] }); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	answers := readAnswers(t, out.String())
	mustEqual(t, len(answers), 1)
	mustEqual(t, answers["1"]["result"].(map[string]any)["protocolVersion"], "2025-06-18")
	mustEqual(t, u.count(), 1) // the notification is not forwarded
	req, _ := u.last()
	mustEqual(t, req.Header.Get("X-API-Key"), testKey)
	if errOut.Len() != 0 {
		t.Fatalf("unexpected log output: %s", errOut.String())
	}
}

func TestRunFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	none := func(string) string { return "" }
	mustEqual(t, run([]string{"-version"}, strings.NewReader(""), &out, &errOut, none), 0)
	mustEqual(t, out.String(), "rapidworkers-mcp "+rapidworkers.Version+"\n")

	out.Reset()
	mustEqual(t, run([]string{"--help"}, strings.NewReader(""), &out, &errOut, none), 0)
	if !strings.Contains(out.String(), "RAPIDWORKERS_API_KEY") {
		t.Fatalf("help: %q", out.String())
	}

	out.Reset()
	mustEqual(t, run([]string{"-nope"}, strings.NewReader(""), &out, &errOut, none), 2)
	mustEqual(t, out.String(), "") // stdout stays clean for the MCP client
}

// TestStdioWithoutKeyEndToEnd builds the command and talks to it over real
// stdio with no API key and no network: what registries and inspectors see.
func TestStdioWithoutKeyEndToEnd(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "rapidworkers-mcp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command(goTool, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	lines := []json.RawMessage{
		rpc("initialize", 1, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}),
		json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
		rpc("tools/list", 2, nil),
		rpc("tools/call", 3, map[string]any{"name": "get_account", "arguments": map[string]any{}}),
	}
	var stdin bytes.Buffer
	for _, line := range lines {
		stdin.Write(line)
		stdin.WriteByte('\n')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stdin = &stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, rapidworkers.APIKeyEnv+"=") && !strings.HasPrefix(kv, rapidworkers.BaseURLEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, rapidworkers.BaseURLEnv+"=http://127.0.0.1:9/") // never the real server
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}

	answers := map[float64]map[string]any{}
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var answer map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &answer); err != nil {
			t.Fatalf("stdout line is not JSON: %q", scanner.Text())
		}
		answers[answer["id"].(float64)] = answer
	}
	if len(answers) != 3 {
		t.Fatalf("answers for ids %v, want 1, 2 and 3", answers)
	}
	init := answers[1]["result"].(map[string]any)
	mustEqual(t, init["serverInfo"].(map[string]any)["name"], "rapidworkers")
	mustEqual(t, init["protocolVersion"], "2025-06-18")
	mustEqual(t, len(answers[2]["result"].(map[string]any)["tools"].([]any)), 22)
	call := answers[3]["result"].(map[string]any)
	mustEqual(t, call["isError"], true)
	if text := call["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "RAPIDWORKERS_API_KEY") {
		t.Fatalf("tool call text: %q", text)
	}
	if !strings.Contains(stderr.String(), "not set") {
		t.Fatalf("stderr: %q", stderr.String())
	}
}
