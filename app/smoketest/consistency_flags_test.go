package main

import (
	"errors"
	"strings"
	"testing"
)

// The flag row against a stub: what the two surfaces must agree on is the SET
// of flags on one message, with the system ones mapped and the custom ones
// carried through untouched.
func TestFlagRowJudgement(t *testing.T) {
	imapSide := func(flags ...string) *reading {
		return newReading(surfIMAP).set("flags", storedFlags(flags))
	}
	jmapSide := func(keywords ...string) *reading {
		return newReading(surfJMAP).set("flags", keywords)
	}

	tests := []struct {
		name   string
		left   *reading
		right  *reading
		wantIn string // "" = must agree
	}{
		{
			name:  "system flags mapped, custom carried through",
			left:  imapSide(`\Seen`, `\Flagged`, consistencyCustomFlag),
			right: jmapSide("$seen", "$flagged", consistencyCustomFlag),
		},
		{
			name:  "session state is not a stored flag",
			left:  imapSide(`\Seen`, `\Recent`),
			right: jmapSide("$seen"),
		},
		{
			// The row exists for this: a mapping table that knows the standard
			// list and drops everything else reads as healthy on system flags.
			name:   "a custom flag the other surface lost",
			left:   imapSide(`\Seen`, consistencyCustomFlag),
			right:  jmapSide("$seen"),
			wantIn: consistencyCustomFlag,
		},
		{
			name:   "a flag mapped to the wrong keyword",
			left:   imapSide(`\Seen`),
			right:  jmapSide("$draft"),
			wantIn: "$draft",
		},
		{
			name:   "a keyword the other surface never set",
			left:   imapSide(`\Seen`),
			right:  jmapSide("$seen", "$answered"),
			wantIn: "$answered",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := judgeRow("flag visibility", tc.left, tc.right, defaultAllowances())
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("agreeing surfaces were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the disagreement was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("verdict %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// The JMAP write path does not exist yet (#712), so the row's second direction
// answers "not checkable" rather than failing: a row that can never pass is a
// red gate that says nothing, and the direction that can be checked is checked
// above it. Any other JMAP error still fails the row — "not implemented" is a
// specific answer, not a category for everything that went wrong.
func TestUnknownMethodIsDistinguishedFromAnyOtherFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"the method does not exist", errors.New(`Email/set: {"type":"unknownMethod"}`), true},
		{"the call was refused", errors.New(`Email/set: {"type":"invalidArguments"}`), false},
		{"the transport failed", errors.New("post https://host: connection refused"), false},
		{"no error at all", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJMAPUnknownMethod(tc.err); got != tc.want {
				t.Errorf("isJMAPUnknownMethod(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
