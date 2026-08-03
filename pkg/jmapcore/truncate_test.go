package jmapcore

import (
	"testing"
	"unicode/utf8"
)

// §4.2.2 forbids splitting a multi-octet character, so the cut lands on a rune
// boundary even when the limit falls inside one.
func TestTruncateBodyKeepsUTF8Intact(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		limit         uint32
		want          string
		wantTruncated bool
	}{
		{name: "under the limit", in: "hello", limit: 10, want: "hello"},
		{name: "exactly the limit", in: "hello", limit: 5, want: "hello"},
		{name: "no limit", in: "hello", limit: 0, want: "hello"},
		{name: "ascii cut", in: "hello world", limit: 5, want: "hello", wantTruncated: true},
		// "привіт" is two bytes per rune: a cut at 5 lands mid-rune.
		{name: "cut inside a rune", in: "привіт", limit: 5, want: "пр", wantTruncated: true},
		{name: "cut on a rune boundary", in: "привіт", limit: 4, want: "пр", wantTruncated: true},
		// An emoji is four bytes; every cut inside it must drop the whole rune.
		{name: "cut inside an emoji", in: "a🙂b", limit: 3, want: "a", wantTruncated: true},
		{name: "cut just after an emoji", in: "a🙂b", limit: 5, want: "a🙂", wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := TruncateBody(tt.in, tt.limit)
			if got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %t, want %t", truncated, tt.wantTruncated)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if tt.limit > 0 && uint32(len(got)) > tt.limit {
				t.Errorf("result is %d bytes, over the %d limit", len(got), tt.limit)
			}
		})
	}
}

// A cut inside a tag would leave markup a client renders as text, or an
// unterminated element swallowing what follows.
func TestTruncateHTMLDropsAPartialTag(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		limit         uint32
		want          string
		wantTruncated bool
	}{
		{name: "under the limit", in: "<p>hi</p>", limit: 100, want: "<p>hi</p>"},
		{name: "cut inside a tag", in: "<p>hi</p><div class=x>", limit: 14, want: "<p>hi</p>", wantTruncated: true},
		{name: "cut after a closed tag", in: "<p>hi</p>tail", limit: 11, want: "<p>hi</p>ta", wantTruncated: true},
		{name: "cut at the opening bracket", in: "<p>hi</p><b>", limit: 10, want: "<p>hi</p>", wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := TruncateHTML(tt.in, tt.limit)
			if got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %t, want %t", truncated, tt.wantTruncated)
			}
		})
	}
}

// The ceiling is the operator's bound on work, so a client that names no cap
// gets it rather than everything, and a client asking for more is held to it.
func TestEffectiveBodyBytes(t *testing.T) {
	tests := []struct {
		name            string
		clientMax, ceil uint32
		want            uint32
	}{
		{name: "client under the ceiling", clientMax: 100, ceil: 1000, want: 100},
		{name: "client over the ceiling", clientMax: 5000, ceil: 1000, want: 1000},
		{name: "client names none", clientMax: 0, ceil: 1000, want: 1000},
		{name: "no ceiling configured", clientMax: 100, ceil: 0, want: 100},
		{name: "neither", clientMax: 0, ceil: 0, want: 0},
		{name: "equal", clientMax: 1000, ceil: 1000, want: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveBodyBytes(tt.clientMax, tt.ceil); got != tt.want {
				t.Errorf("= %d, want %d", got, tt.want)
			}
		})
	}
}
