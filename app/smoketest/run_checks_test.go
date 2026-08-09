package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A check the deployment did not configure must stay visible and must not be
// counted as a pass: a summary built from what ran cannot report what was
// never asked for (#1197). And -require-all is what makes the report
// load-bearing -- without it the honest line lands in output nobody reads.
func TestRunChecksReportsSkipsAndHonoursRequireAll(t *testing.T) {
	run := func(t *testing.T, checks []check, requireAll bool, want summary) string {
		t.Helper()
		var out bytes.Buffer
		if got := runChecks(checks, requireAll, &out); got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
		return out.String()
	}

	t.Run("a skip is reported, is not a pass, and does not run", func(t *testing.T) {
		ran := false
		// The skipped item carries an fn that would record its own execution,
		// so "did not run" is a claim the case can fail.
		set := []check{
			{name: "configured ok", fn: func() error { return nil }},
			{name: "configured failing", fn: func() error { return errors.New("boom") }},
			{name: "fts search", skip: "needs -envelope-user", fn: func() error { ran = true; return nil }},
		}
		out := run(t, set, false, summary{total: 3, passed: 1, failed: 1, skipped: 1})
		if ran {
			t.Error("a skipped check was executed")
		}
		if n := strings.Count(out, "fts search"); n != 1 {
			t.Errorf("skipped check named %d times in the report, want 1:\n%s", n, out)
		}
		if !strings.Contains(out, "needs -envelope-user") {
			t.Errorf("report does not name the flag that would enable the check:\n%s", out)
		}
	})

	t.Run("a skip alone does not fail the run", func(t *testing.T) {
		only := []check{{name: "fts search", skip: "needs -fts-user"}}
		run(t, only, false, summary{total: 1, passed: 0, failed: 0, skipped: 1})
	})

	t.Run("require-all turns a skip into a failure, itemised once", func(t *testing.T) {
		only := []check{{name: "fts search", skip: "needs -fts-user"}}
		out := run(t, only, true, summary{total: 1, passed: 0, failed: 1, skipped: 1})
		if n := strings.Count(out, "fts search"); n != 1 {
			t.Errorf("skipped check listed %d times under -require-all, want 1:\n%s", n, out)
		}
	})

	t.Run("a fully configured green run still passes under require-all", func(t *testing.T) {
		green := []check{{name: "configured ok", fn: func() error { return nil }}}
		if out := run(t, green, true, summary{total: 1, passed: 1, failed: 0, skipped: 0}); out != "" {
			t.Errorf("green run reported problems:\n%s", out)
		}
	})
}
