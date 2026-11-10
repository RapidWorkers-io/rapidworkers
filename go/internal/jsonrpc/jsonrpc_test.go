package jsonrpc

import (
	"encoding/json"
	"testing"
)

func TestFind(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, id, want string
	}{
		{"object, any id", "application/json", `{"id":null,"error":{}}`, `1`, `null`},
		{"batch by id", "application/json", `[{"id":2,"r":1},{"id":1,"r":2}]`, `1`, `1`},
		{"batch without match", "application/json", `[{"id":2}]`, `1`, ""},
		{"not JSON", "text/html", `<html>`, `1`, ""},
		{"empty", "application/json", ``, `1`, ""},
		{"sse", "text/event-stream; charset=utf-8", "event: message\ndata: {\"id\":\"a\"}\n\n", `"a"`, `"a"`},
		{"sse skips other ids", "text/event-stream", "data: {\"id\":2}\n\ndata: {\"id\":1}\n\n", `1`, `1`},
		{"sse crlf, no final blank line", "text/event-stream", "event: message\r\ndata: {\"id\":1}", `1`, `1`},
		{"sse data split over lines", "text/event-stream", "data: {\"id\":\ndata: 1}\n\n", `1`, `1`},
		{"sse one message per data line", "text/event-stream", "data: {\"id\":2}\ndata: {\"id\":1}\n\n", `1`, `1`},
		{"sse number ids compare as numbers", "text/event-stream", "data: {\"id\":1.0}\n\n", `1`, `1.0`},
		{"sse string is not number", "text/event-stream", "data: {\"id\":\"1\"}\n\n", `1`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Find(tc.contentType, []byte(tc.body), json.RawMessage(tc.id))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil || string(got["id"]) != tc.want {
				t.Fatalf("got %v, want the message with id %s", got, tc.want)
			}
		})
	}
}

func TestSameID(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`1`, `1`, true},
		{`1`, `1.0`, true},
		{`1`, `"1"`, false},
		{`"abc"`, `"abc"`, true},
		{`null`, ``, true},
		{`null`, `0`, false},
	} {
		if got := SameID(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Errorf("SameID(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
}

func TestString(t *testing.T) {
	if v, ok := String(json.RawMessage(` "2.0"`)); !ok || v != "2.0" {
		t.Fatalf("String = %q, %v", v, ok)
	}
	for _, raw := range []string{`null`, `2`, ``, `{"a":1}`} {
		if _, ok := String(json.RawMessage(raw)); ok {
			t.Errorf("String(%s) reported a string", raw)
		}
	}
}

func TestMarshalKeepsHTMLCharacters(t *testing.T) {
	data, err := Marshal(map[string]string{"a": "<b>&"})
	if err != nil || string(data) != `{"a":"<b>&"}` {
		t.Fatalf("Marshal = %s, %v", data, err)
	}
}
