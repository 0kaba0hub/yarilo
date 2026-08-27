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

// The two directions are two rows, and the one that cannot run yet has to be
// visible as a skip naming why. A single row covering both would pass on the
// half that works and say nothing about the half that does not exist — the
// softer form of the missing pair this area exists to prevent.
func TestFlagDirectionsAreSeparateRows(t *testing.T) {
	// With the surfaces configured, or the rows are all skipped and every
	// assertion about them holds by accident. The first version of this guard
	// compared the two skips and passed on a mutation that turned the back
	// direction off, because with no flags set both were off already.
	withConsistencySurfaces(t)

	var checks []check
	registerConsistency(&checks)

	var forward, back *check
	for i := range checks {
		switch {
		case strings.Contains(checks[i].name, "imap->jmap flag"):
			forward = &checks[i]
		case strings.Contains(checks[i].name, "jmap->imap keyword"):
			back = &checks[i]
		}
	}
	if forward == nil || back == nil {
		t.Fatalf("both directions must be registered; forward=%v back=%v", forward != nil, back != nil)
	}
	// Both directions are gated the same way now that Email/set exists
	// (#1216). This used to require the back direction to be a NAMED SKIP,
	// which was right while the write path did not exist and wrong the day it
	// did -- the row stayed off for a release after the feature it needed had
	// shipped.
	//
	// So the assertion is that the two directions are gated alike: whatever
	// turns one off must turn the other off, and nothing may turn off the back
	// direction alone.
	// Both must RUN, not merely be gated alike: Email/set exists (#1216), so
	// nothing may hold the back direction off on its own. It stayed off for a
	// release after the feature it needed had shipped, and the report showed
	// that as a named skip rather than as a gap.
	if forward.skip != "" {
		t.Errorf("the imap->jmap direction is skipped with every surface configured: %q", forward.skip)
	}
	if back.skip != "" {
		t.Errorf("the jmap->imap direction is skipped with every surface configured: %q — "+
			"Email/set is implemented, so this direction goes unchecked while the report shows a skip", back.skip)
	}
}

// withConsistencySurfaces configures the surfaces the consistency rows gate on
// and restores them afterwards.
func withConsistencySurfaces(t *testing.T) {
	t.Helper()
	prevJMAP, prevUser, prevPass, prevPort := *flagJMAP, *flagJMAPUser, *flagJMAPPass, *flagDeliveryPort
	*flagJMAP, *flagJMAPUser, *flagJMAPPass, *flagDeliveryPort = true, "u1@example.test", "pw", "24"
	t.Cleanup(func() {
		*flagJMAP, *flagJMAPUser, *flagJMAPPass, *flagDeliveryPort = prevJMAP, prevUser, prevPass, prevPort
	})
}
