package main

import (
	"encoding/json"
	"testing"
)

func TestAppendJSONMatchesEncodingJSON(t *testing.T) {
	vals := []any{
		nil, "", "plain", "q\"b\\s\n\r\t\b\f\x01\x1f<>&", "uni é 日本    \xff\xfe bad", true, false, 0, -42, int64(1 << 60), 3.25, 1e21,
		map[string]any{"b": 1, "a": []any{"x", nil, map[string]any{"z": false}}, "c": []string{"s", "<t>"}, "d": []map[string]any{{"k": "v"}}, "e": map[string]any(nil)},
		[]any(nil), []string(nil), struct{ A int }{3},
	}
	for _, v := range vals {
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := appendJSON(nil, v)
		if err != nil || string(got) != string(want) {
			t.Fatalf("%#v: got %s want %s (%v)", v, got, want, err)
		}
	}
}

func TestRequestID(t *testing.T) {
	s := &Server{idPrefix: "F0RG:0001:"}
	s.reqSeq.Store(0x12345678 - 1)
	if got := s.requestID(); got != "F0RG:0001:000012:12345678" {
		t.Fatal(got)
	}
}
