package main

import (
	"strings"
	"testing"
)

// A row whose second surface is absent is a skip that NAMES it — not a pass,
// and not a silent omission. A pair that quietly disappears when a surface is
// undeployed is exactly the coverage hole this area exists to close (#1197,
// #1209).
func TestRowGateNamesWhatIsMissing(t *testing.T) {
	imapOn := surfaceState{surface: surfIMAP, present: true, needs: "-imap-user"}
	imapOff := surfaceState{surface: surfIMAP, present: false, needs: "-imap-user"}
	jmapOff := surfaceState{surface: surfJMAP, present: false, needs: "-jmap"}
	jmapOn := surfaceState{surface: surfJMAP, present: true, needs: "-jmap"}

	tests := []struct {
		name        string
		sides       []surfaceState
		wantEnabled bool
		wantNamed   []string
	}{
		{"both sides configured", []surfaceState{imapOn, jmapOn}, true, nil},
		{"second side missing", []surfaceState{imapOn, jmapOff}, false, []string{"jmap", "-jmap"}},
		{"first side missing", []surfaceState{imapOff, jmapOn}, false, []string{"imap", "-imap-user"}},
		{"neither side", []surfaceState{imapOff, jmapOff}, false,
			[]string{"imap", "-imap-user", "jmap", "-jmap"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enabled, skip := rowGate(tc.sides...)
			if enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v (skip %q)", enabled, tc.wantEnabled, skip)
			}
			if enabled {
				if skip != "" {
					t.Errorf("an enabled row carries a skip: %q", skip)
				}
				return
			}
			if skip == "" {
				t.Fatal("a disabled row carries no skip: it would vanish from the report")
			}
			for _, want := range tc.wantNamed {
				if !strings.Contains(skip, want) {
					t.Errorf("skip %q does not name %q", skip, want)
				}
			}
		})
	}
}

// The rows are registered in the ordinary machinery, so -require-all and
// -require-all-except keep working on them without a special case: the area is
// a name in the same list every other area is in.
func TestConsistencyRowsAreOrdinaryChecks(t *testing.T) {
	var found int
	for _, c := range register() {
		if c.area != consistencyArea {
			continue
		}
		found++
		if c.name == "" {
			t.Error("a consistency row registered without a name")
		}
		if c.fn == nil && c.skip == "" {
			t.Errorf("row %q is neither runnable nor skipped", c.name)
		}
		if c.fn != nil && c.skip != "" {
			t.Errorf("row %q is both runnable and skipped", c.name)
		}
	}
	if found == 0 {
		t.Fatal("no consistency rows registered: the area cannot appear in the report")
	}
}

// The rows phase 2 (#1216) made possible must be registered and must run.
//
// State and incremental changes were served by a released build and never
// called in the field, and the write direction stayed a named skip for a
// release after the feature it needed had shipped. Both are the same failure:
// the report showed a skip where there was a gap, and a skip reads as a
// decision.
func TestThePhaseTwoRowsAreRegisteredAndRun(t *testing.T) {
	withConsistencySurfaces(t)

	var checks []check
	registerConsistency(&checks)

	for _, want := range []string{"Email/changes", "jmap->imap keyword"} {
		var found *check
		for i := range checks {
			if strings.Contains(checks[i].name, want) {
				found = &checks[i]
				break
			}
		}
		if found == nil {
			t.Errorf("no consistency row covers %q; it is served and nothing asks it anything", want)
			continue
		}
		if found.skip != "" {
			t.Errorf("the row covering %q is skipped with every surface configured: %q", want, found.skip)
		}
	}
}
