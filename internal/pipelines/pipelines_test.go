package pipelines

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A pull request into either line must reach the gate, and neither may filter
// on paths.
//
// Both halves matter, and for opposite reasons. A PR into develop used to run
// nothing and was green because nothing had run. And once Lint and Test are
// required checks, a paths filter is worse than that: a docs-only PR starts no
// run, so the check never reports and the pull request sits at "expected" for
// ever. A job that is skipped counts as passed; a job that never starts does
// not.
//
// So the decision moved into gate.yml, where one list serves both lines instead
// of two lists that agree by inspection.
func TestNeitherLineFiltersPullRequestsByPath(t *testing.T) {
	for _, name := range []string{"ci.yml", "ci-develop.yml"} {
		on := triggers(t, name)
		pr, ok := on["pull_request"].(map[string]any)
		if !ok {
			t.Errorf("%s has no pull_request trigger, so a PR against it runs nothing and is green because nothing ran", name)
			continue
		}
		if paths, ok := pr["paths"]; ok {
			t.Errorf("%s filters pull requests by path (%v); with Lint and Test required, a change outside that list would never report and the PR could never merge",
				name, paths)
		}
	}
}

// And the gate itself must still decide, or removing the filters above would
// simply run everything on every documentation change.
func TestTheGateDecidesWhetherThereIsCodeToCheck(t *testing.T) {
	gate := readWorkflow(t, "gate.yml")

	var doc struct {
		Jobs map[string]struct {
			Needs any    `yaml:"needs"`
			If    string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(gate), &doc); err != nil {
		t.Fatalf("parse gate.yml: %v", err)
	}
	if _, ok := doc.Jobs["changes"]; !ok {
		t.Fatal("gate.yml has no `changes` job; nothing decides whether the gate has work, so every docs PR runs the whole suite")
	}
	for _, name := range []string{"lint", "test"} {
		job, ok := doc.Jobs[name]
		if !ok {
			t.Errorf("gate.yml has no %q job; this guard is reading a file that changed shape", name)
			continue
		}
		if !strings.Contains(job.If, "changes.outputs.code") {
			t.Errorf("job %q does not depend on the changes decision (if: %q), so it runs on every pull request", name, job.If)
		}
	}

	// The list is in that job and nowhere else. Both push triggers still carry
	// their own paths -- a push has no base to diff against -- so those two are
	// compared with it rather than left to drift.
	want := pathsOf(t, gate)
	if len(want) < 5 {
		t.Fatalf("found only %d paths in gate.yml's filter; the guard is reading the wrong thing", len(want))
	}
	for _, name := range []string{"ci.yml", "ci-develop.yml"} {
		push, _ := triggers(t, name)["push"].(map[string]any)
		got := map[string]bool{}
		for _, p := range push["paths"].([]any) {
			got[strings.TrimSuffix(strings.TrimSuffix(p.(string), "/**"), "$")] = true
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s does not gate pushes touching %q, which gate.yml treats as code", name, w)
			}
		}
	}
}

// pathsOf pulls the prefixes out of the grep the changes job runs, so the guard
// reads the same string the pipeline does rather than a copy of it.
func pathsOf(t *testing.T, gate string) []string {
	t.Helper()
	m := regexp.MustCompile(`grep -qE '\^\(([^)]+)\)`).FindStringSubmatch(gate)
	if m == nil {
		t.Fatal("cannot find the path pattern in gate.yml's changes job")
	}
	var out []string
	for _, p := range strings.Split(m[1], "|") {
		p = strings.TrimSuffix(strings.TrimSuffix(p, "$"), "/")
		out = append(out, strings.ReplaceAll(p, `\.`, "."))
	}
	return out
}

func triggers(t *testing.T, name string) map[string]any {
	t.Helper()
	var doc map[any]any
	if err := yaml.Unmarshal([]byte(readWorkflow(t, name)), &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	// "on" is the boolean true under YAML 1.1 rules, which is why this is a
	// map of any rather than a struct tag.
	on, ok := doc[true].(map[string]any)
	if !ok {
		if on, ok = doc["on"].(map[string]any); !ok {
			t.Fatalf("%s has no on: block this guard can read", name)
		}
	}
	return on
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// Every workflow that triggers on a branch must name develop, or it stopped
// working the day the work moved there.
//
// secret-scan did: it ran on pushes to main only, so from the move to develop
// until the first cut nothing scanned the commits people actually write. A
// secret would have been found in the history rather than in the change that
// introduced it, which is the difference between deleting a line and rotating a
// credential.
//
// ci.yml is the exception and stays one: it is the master line, and develop has
// its own pipeline. Named here rather than inferred, so adding a workflow means
// deciding rather than defaulting.
func TestEveryBranchTriggeredWorkflowCoversDevelop(t *testing.T) {
	masterOnly := map[string]string{
		"ci.yml":                     "the master pipeline; develop has ci-develop.yml",
		"dockerhub-description.yaml": "publishes the description of what master released",
	}
	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	var checked int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		on := triggers(t, name)
		push, ok := on["push"].(map[string]any)
		if !ok {
			continue
		}
		branches, ok := push["branches"].([]any)
		if !ok {
			continue
		}
		checked++
		var names []string
		for _, b := range branches {
			names = append(names, b.(string))
		}
		if why, exempt := masterOnly[name]; exempt {
			if slices.Contains(names, "develop") {
				t.Errorf("%s is listed as master-only (%s) but triggers on develop", name, why)
			}
			continue
		}
		if !slices.Contains(names, "develop") {
			t.Errorf("%s triggers on %v and not on develop, where the work lands: it stopped running the day the work moved", name, names)
		}
	}
	if checked < 3 {
		t.Errorf("found %d branch-triggered workflows; this guard is reading the wrong directory", checked)
	}
}
