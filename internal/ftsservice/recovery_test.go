package ftsservice

import "testing"

// TestIsBrokenEngine locks the error classifier that drives handle eviction
// (#629): a Xapian closed/opening error, or the rev-file write failure that
// wedges a flatcurve shard, must be recognised so the worker reopens the index;
// an ordinary error must not trigger a needless reopen.
func TestIsBrokenEngine(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errString("fts/flatcurve: term too long"), false},
		{"database closed", errString("fts/flatcurve: DatabaseClosedError: Database has been closed"), true},
		{"opening error", errString("fts/flatcurve: DatabaseOpeningError: Couldn't write new rev file: .../v.tmp (No such file or directory)"), true},
		{"rev file write", errString("Couldn't write new rev file: current.123/v.tmp"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := brokenEngineReason(c.err) != ""; got != c.want {
				t.Errorf("brokenEngineReason(%v) present = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
