package main

import (
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
