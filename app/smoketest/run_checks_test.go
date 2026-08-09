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
	assert := func(t *testing.T, got summary, want summary) {
		t.Helper()
		if got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	}

	t.Run("a skip is reported, is not a pass, and does not run", func(t *testing.T) {
		ran := false
		// The skipped item carries an fn that would record its own execution,
		// so "did not run" is a claim the case can fail.
		set := []check{
			{name: "configured ok", fn: func() error { return nil }},
			{name: "configured failing", fn: func() error { return errors.New("boom") }},
			{name: "not configured", skip: "needs -envelope-user", fn: func() error { ran = true; return nil }},
		}
		assert(t, runChecks(set, false), summary{total: 3, passed: 1, failed: 1, skipped: 1})
		if ran {
			t.Error("a skipped check was executed")
		}
	})

	t.Run("a skip alone does not fail the run", func(t *testing.T) {
		only := []check{{name: "not configured", skip: "needs -fts-user"}}
		assert(t, runChecks(only, false), summary{total: 1, passed: 0, failed: 0, skipped: 1})
	})

	t.Run("require-all turns a skip into a failure", func(t *testing.T) {
		only := []check{{name: "not configured", skip: "needs -fts-user"}}
		assert(t, runChecks(only, true), summary{total: 1, passed: 0, failed: 1, skipped: 1})
	})

	t.Run("a fully configured green run still passes under require-all", func(t *testing.T) {
		green := []check{{name: "configured ok", fn: func() error { return nil }}}
		assert(t, runChecks(green, true), summary{total: 1, passed: 1, failed: 0, skipped: 0})
	})
}
