package jmapcore

import (
	"encoding/json"
	"testing"
)

// The wire form is a three-element array. A struct-shaped encoding would look
// fine in Go and be unreadable to every client.
func TestInvocationRoundTrip(t *testing.T) {
	in := Invocation{Name: "Core/echo", Args: json.RawMessage(`{"hello":"world"}`), CallID: "c0"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `["Core/echo",{"hello":"world"},"c0"]` {
		t.Fatalf("wire form = %s", raw)
	}
	var out Invocation
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != in.Name || out.CallID != in.CallID || string(out.Args) != string(in.Args) {
		t.Errorf("round trip lost data: %+v", out)
	}
}

// Absent arguments marshal as an empty object, not null: a client reading
// args.foo on null crashes.
func TestInvocationEmptyArgs(t *testing.T) {
	raw, err := json.Marshal(Invocation{Name: "Core/echo", CallID: "c0"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `["Core/echo",{},"c0"]` {
		t.Errorf("wire form = %s", raw)
	}
}

// Anything that is not exactly [name, args, callId] is malformed: the server
// cannot tell which call a response would answer.
func TestInvocationRejectsMalformedTriples(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"object", `{"name":"Core/echo"}`},
		{"two elements", `["Core/echo",{}]`},
		{"four elements", `["Core/echo",{},"c0","extra"]`},
		{"empty array", `[]`},
		{"name not a string", `[7,{},"c0"]`},
		{"call id not a string", `["Core/echo",{},7]`},
		{"args not an object", `["Core/echo",[],"c0"]`},
		{"args null", `["Core/echo",null,"c0"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var inv Invocation
			if err := json.Unmarshal([]byte(tt.body), &inv); err == nil {
				t.Errorf("accepted %s as an invocation: %+v", tt.body, inv)
			}
		})
	}
}
