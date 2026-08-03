package jmapcore

import "testing"

// The ceiling bounds the server's work, so a client that named no limit gets it
// rather than an unbounded result set (RFC 8620 §5.5 permits the server's own
// limit provided the response reports it). Same shape as EffectiveBodyBytes.
func TestEffectiveLimit(t *testing.T) {
	u := func(n uint) *uint { return &n }
	tests := []struct {
		name    string
		client  *uint
		ceiling uint
		want    uint
	}{
		{name: "client under the ceiling", client: u(10), ceiling: 100, want: 10},
		{name: "client over the ceiling", client: u(5000), ceiling: 100, want: 100},
		{name: "client names none", client: nil, ceiling: 100, want: 100},
		{name: "no ceiling configured", client: u(10), ceiling: 0, want: 10},
		{name: "neither", client: nil, ceiling: 0, want: 0},
		{name: "client asks for zero", client: u(0), ceiling: 100, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveLimit(tt.client, tt.ceiling); got != tt.want {
				t.Errorf("= %d, want %d", got, tt.want)
			}
		})
	}
}

// A refusal must name the conditions it could not honour, so a client can drop
// them and retry rather than guessing which part was unsupported.
func TestTextConditionsAreNamed(t *testing.T) {
	s := "x"
	f := &EmailFilter{Subject: &s, Body: &s, Header: []string{"X-Foo"}}
	got := f.TextConditions()
	want := []string{"body", "header", "subject"}
	if len(got) != len(want) {
		t.Fatalf("= %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("= %v, want %v (sorted, so the message is stable)", got, want)
		}
	}
	if (&EmailFilter{}).TextConditions() != nil {
		t.Error("an index-only filter must report no text conditions")
	}
}
