package jmapcore

import (
	"encoding/json"
	"testing"
)

// JSON Pointer, including the escape order of RFC 6901 §3: ~1 before ~0, or
// "~01" would decode to "/" instead of "~1".
func TestEvalPointer(t *testing.T) {
	doc := json.RawMessage(`{
		"list": [{"id":"a"},{"id":"b"}],
		"ids": ["x","y"],
		"nested": {"a/b": 1, "c~d": 2, "~1": 3},
		"groups": [{"ids":["p","q"]},{"ids":["r"]}]
	}`)
	tests := []struct {
		name, path, want string
		wantErr          bool
	}{
		{name: "whole document", path: "", want: string(doc)},
		{name: "member", path: "/ids", want: `["x","y"]`},
		{name: "array index", path: "/list/1/id", want: `"b"`},
		{name: "star over objects", path: "/list/*/id", want: `["a","b"]`},
		{name: "star flattens one level", path: "/groups/*/ids", want: `["p","q","r"]`},
		{name: "escaped slash", path: "/nested/a~1b", want: `1`},
		{name: "escaped tilde", path: "/nested/c~0d", want: `2`},
		{name: "escape order", path: "/nested/~01", want: `3`},
		{name: "missing member", path: "/nope", wantErr: true},
		{name: "index out of range", path: "/list/999/id", wantErr: true},
		{name: "index on an object", path: "/nested/0", wantErr: true},
		{name: "star on a non-array", path: "/nested/*", wantErr: true},
		{name: "no leading slash", path: "ids", wantErr: true},
		{name: "into a scalar", path: "/ids/0/deeper", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalPointer(doc, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("path %q resolved to %s, want an error", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("path %q: %v", tt.path, err)
			}
			if !jsonEqual(t, got, tt.want) {
				t.Errorf("path %q = %s, want %s", tt.path, got, tt.want)
			}
		})
	}
}

// A reference resolves against the responses produced so far, which is what
// makes every failure mode below a property of the design rather than a check.
func TestResolveBackRefs(t *testing.T) {
	done := []Invocation{
		{Name: "Foo/query", Args: json.RawMessage(`{"ids":["m1","m2"]}`), CallID: "c0"},
		{Name: "error", Args: json.RawMessage(`{"type":"serverFail"}`), CallID: "c1"},
	}
	tests := []struct {
		name    string
		args    string
		want    string
		wantErr string
	}{
		{
			name: "resolves into the named result",
			args: `{"#ids":{"resultOf":"c0","name":"Foo/query","path":"/ids"}}`,
			want: `{"ids":["m1","m2"]}`,
		},
		{
			name: "leaves plain arguments alone",
			args: `{"ids":["m9"]}`,
			want: `{"ids":["m9"]}`,
		},
		{
			name:    "reference to a call that failed",
			args:    `{"#ids":{"resultOf":"c1","name":"Foo/query","path":"/ids"}}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "forward reference",
			args:    `{"#ids":{"resultOf":"c2","name":"Foo/query","path":"/ids"}}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "wrong method name",
			args:    `{"#ids":{"resultOf":"c0","name":"Bar/query","path":"/ids"}}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "path that does not resolve",
			args:    `{"#ids":{"resultOf":"c0","name":"Foo/query","path":"/ids/999"}}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "star over a non-array",
			args:    `{"#ids":{"resultOf":"c0","name":"Foo/query","path":"/ids/0/*"}}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "reference is not a reference object",
			args:    `{"#ids":"c0"}`,
			wantErr: ErrInvalidResultReference,
		},
		{
			name:    "given both ways",
			args:    `{"ids":["m9"],"#ids":{"resultOf":"c0","name":"Foo/query","path":"/ids"}}`,
			wantErr: ErrInvalidArguments,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, merr := resolveBackRefs(json.RawMessage(tt.args), done)
			if tt.wantErr != "" {
				if merr == nil {
					t.Fatalf("resolved to %s, want %s", got, tt.wantErr)
				}
				if merr.Type != tt.wantErr {
					t.Errorf("error type = %s, want %s", merr.Type, tt.wantErr)
				}
				return
			}
			if merr != nil {
				t.Fatalf("unexpected error: %v", merr)
			}
			if !jsonEqual(t, got, tt.want) {
				t.Errorf("= %s, want %s", got, tt.want)
			}
		})
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("got is not JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want is not JSON: %s", want)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
