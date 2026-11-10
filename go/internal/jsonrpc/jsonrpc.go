// Package jsonrpc finds the JSON-RPC answer in a response from the hosted
// RapidWorkers MCP server. It is shared by the SDK and the stdio relay.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// Message is a decoded JSON-RPC message with its members kept raw.
type Message map[string]json.RawMessage

// Find returns the JSON-RPC message answering id from a JSON or
// Server-Sent Events response body, or nil when there is none.
//
// A JSON object body is the answer whatever its id (error answers may carry a
// null id). In a JSON array or an event stream only a message with the same
// id counts.
func Find(contentType string, body []byte, id json.RawMessage) Message {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return findEvent(body, id)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	switch body[0] {
	case '{':
		var message Message
		if json.Unmarshal(body, &message) != nil {
			return nil
		}
		return message
	case '[':
		var batch []json.RawMessage
		if json.Unmarshal(body, &batch) != nil {
			return nil
		}
		for _, item := range batch {
			if message := object(item); message != nil && SameID(message["id"], id) {
				return message
			}
		}
	}
	return nil
}

// findEvent reads the data of each event in an SSE stream. The data lines of
// one event are joined as the SSE format says; each line is also tried on its
// own, so a stream with one JSON message per data line works too.
func findEvent(body []byte, id json.RawMessage) Message {
	var data []string
	check := func() Message {
		defer func() { data = data[:0] }()
		candidates := []string{strings.Join(data, "\n")}
		if len(data) > 1 {
			candidates = append(candidates, data...)
		}
		for _, candidate := range candidates {
			if message := object([]byte(candidate)); message != nil && SameID(message["id"], id) {
				return message
			}
		}
		return nil
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(body), "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			if len(data) > 0 {
				if message := check(); message != nil {
					return message
				}
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(value))
		}
	}
	if len(data) > 0 {
		return check()
	}
	return nil
}

func object(raw []byte) Message {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var message Message
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	return message
}

// SameID reports whether two raw JSON-RPC ids are equal as JSON values, so 1
// and 1.0 match but 1 and "1" do not. A missing id equals null.
func SameID(a, b json.RawMessage) bool {
	return reflect.DeepEqual(decode(a), decode(b))
}

func decode(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return value
}

// String returns the value of a raw JSON string, and false for any other
// JSON value (including null).
func String(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// Marshal encodes v as compact JSON without escaping <, > and &.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
