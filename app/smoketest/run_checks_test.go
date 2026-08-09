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
	run := func(t *testing.T, checks []check, requireAll bool, exempt map[string]bool, want summary) string {
		t.Helper()
		var out bytes.Buffer
		if got := runChecks(checks, requireAll, exempt, &out); got != want {
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
		out := run(t, set, false, nil, summary{total: 3, passed: 1, failed: 1, skipped: 1})
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
		run(t, only, false, nil, summary{total: 1, passed: 0, failed: 0, skipped: 1})
	})

	t.Run("require-all turns a skip into a failure, itemised once", func(t *testing.T) {
		only := []check{{name: "fts search", skip: "needs -fts-user"}}
		out := run(t, only, true, nil, summary{total: 1, passed: 0, failed: 1, skipped: 1})
		if n := strings.Count(out, "fts search"); n != 1 {
			t.Errorf("skipped check listed %d times under -require-all, want 1:\n%s", n, out)
		}
	})

	t.Run("a fully configured green run still passes under require-all", func(t *testing.T) {
		green := []check{{name: "configured ok", fn: func() error { return nil }}}
		if out := run(t, green, true, nil, summary{total: 1, passed: 1, failed: 0, skipped: 0}); out != "" {
			t.Errorf("green run reported problems:\n%s", out)
		}
	})
}

// -require-all-except narrows the demand to the areas a deployment actually
// runs. It forgives "not configured" in those areas -- never a check that ran
// and failed, and never another area (#1199).
func TestRequireAllExceptForgivesOnlySkipsInNamedAreas(t *testing.T) {
	set := func() []check {
		return []check{
			{area: "jmap", name: "jmap session", skip: "needs -jmap"},
			{area: "imap", name: "imap FTS", skip: "needs -fts-user"},
			{area: "jmap", name: "jmap batch", fn: func() error { return errors.New("boom") }},
		}
	}

	t.Run("an exempt area's skip is forgiven, its failure is not", func(t *testing.T) {
		var out bytes.Buffer
		got := runChecks(set(), true, map[string]bool{"jmap": true}, &out)
		// The imap skip still fails, and so does the jmap check that ran.
		want := summary{total: 3, passed: 0, failed: 2, skipped: 2, exempt: 1}
		if got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
		if !strings.Contains(out.String(), "jmap batch: boom") {
			t.Errorf("exemption swallowed a real failure:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "area jmap exempt") {
			t.Errorf("report does not say why the skip was tolerated:\n%s", out.String())
		}
	})

	t.Run("exempting another area forgives nothing here", func(t *testing.T) {
		var out bytes.Buffer
		got := runChecks(set(), true, map[string]bool{"smtp": true}, &out)
		want := summary{total: 3, passed: 0, failed: 3, skipped: 2, exempt: 0}
		if got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	})

	t.Run("without require-all an exemption changes no outcome", func(t *testing.T) {
		var out bytes.Buffer
		got := runChecks(set(), false, map[string]bool{"jmap": true}, &out)
		want := summary{total: 3, passed: 0, failed: 1, skipped: 2, exempt: 1}
		if got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	})
}

// An area no check declares is a typo, and a typo must not read as a narrower
// gate that silently still demands everything.
func TestParseExemptionsRejectsUnknownAreas(t *testing.T) {
	checks := []check{{area: "jmap", name: "jmap session"}, {area: "imap", name: "imap FTS"}}

	got, err := parseExemptions(" jmap , imap ", checks)
	if err != nil {
		t.Fatalf("parseExemptions: %v", err)
	}
	if len(got) != 2 || !got["jmap"] || !got["imap"] {
		t.Errorf("exempt = %v, want jmap and imap", got)
	}

	if got, err := parseExemptions("", checks); err != nil || len(got) != 0 {
		t.Errorf("empty list = %v, %v; want an empty set and no error", got, err)
	}

	err = func() error { _, err := parseExemptions("jmpa", checks); return err }()
	if err == nil {
		t.Fatal("a misspelled area was accepted")
	}
	if !strings.Contains(err.Error(), "imap, jmap") {
		t.Errorf("error does not list the known areas: %v", err)
	}
}

// The exemption names an area, so every check must declare one, and the name
// must start with it or the report reads as if it belongs elsewhere.
func TestRegisteredChecksDeclareTheirArea(t *testing.T) {
	checks := register()
	if len(checks) == 0 {
		t.Fatal("register() produced no checks")
	}
	areas := map[string]bool{}
	for _, c := range checks {
		if c.area == "" {
			t.Errorf("check %q declares no area", c.name)
			continue
		}
		if !strings.HasPrefix(c.name, c.area) {
			t.Errorf("check %q is in area %q but does not name it", c.name, c.area)
		}
		areas[c.area] = true
	}
	// The case #1199 was filed about: a deployment that does not run JMAP.
	if !areas["jmap"] {
		t.Error("no jmap area, so -require-all-except=jmap would be rejected")
	}
}
