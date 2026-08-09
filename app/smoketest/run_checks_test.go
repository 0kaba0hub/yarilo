package main

import (
	"errors"
	"testing"
)

// A check the deployment did not configure must stay visible and must not be
// counted as a pass: a summary built from what ran cannot report what was
// never asked for (#1197). And -require-all is what makes the report
// load-bearing -- without it the honest line lands in output nobody reads.
func TestRunChecksReportsSkipsAndHonoursRequireAll(t *testing.T) {
	ran := map[string]bool{}
	newSet := func() []check {
		return []check{
			{name: "configured ok", fn: func() error { ran["ok"] = true; return nil }},
			{name: "configured failing", fn: func() error { ran["bad"] = true; return errors.New("boom") }},
			{name: "not configured", skip: "needs -envelope-user"},
		}
	}

	t.Run("a skip is not a pass and does not run", func(t *testing.T) {
		ran = map[string]bool{}
		if failed := runChecks(newSet(), false); !failed {
			t.Error("a failing check did not fail the run")
		}
		if ran["skip"] {
			t.Error("a skipped check was executed")
		}
	})

	t.Run("a skip alone does not fail the run", func(t *testing.T) {
		only := []check{{name: "not configured", skip: "needs -fts-user"}}
		if failed := runChecks(only, false); failed {
			t.Error("a skipped check failed the run without -require-all")
		}
	})

	t.Run("require-all turns a skip into a failure", func(t *testing.T) {
		only := []check{{name: "not configured", skip: "needs -fts-user"}}
		if failed := runChecks(only, true); !failed {
			t.Error("-require-all did not fail on an unconfigured check")
		}
	})

	t.Run("a fully configured green run still passes under require-all", func(t *testing.T) {
		green := []check{{name: "configured ok", fn: func() error { return nil }}}
		if failed := runChecks(green, true); failed {
			t.Error("-require-all failed a run with nothing skipped")
		}
	})
}
