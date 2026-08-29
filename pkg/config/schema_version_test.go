package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// The chart and the binary must name the same schema.
//
// The whole mechanism is two numbers that have to move together, so the failure
// mode is somebody bumping one of them. Nothing else notices: a chart claiming
// a schema it does not render passes lint, and a binary needing a schema the
// chart never reached only says so at start on a real deployment.
func TestTheChartAndTheBinaryAgreeOnTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "helm", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	m := regexp.MustCompile(`(?m)^config_schema_version:\s*(\d+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("values.yaml declares no config_schema_version, so the ConfigMap renders the default and the check is dead")
	}
	fromChart, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("config_schema_version in values.yaml is not a number: %q", m[1])
	}
	if fromChart != minConfigSchema {
		t.Errorf("values.yaml says schema %d and the binary needs %d: one of the two was bumped alone",
			fromChart, minConfigSchema)
	}
}

// The ConfigMap template must actually render it, or the binary reads zero from
// every deployment and warns about a chart that is not behind at all.
func TestTheConfigMapRendersTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "configmap.yaml"))
	if err != nil {
		t.Fatalf("read configmap.yaml: %v", err)
	}
	if !regexp.MustCompile(`config_schema_version:`).Match(raw) {
		t.Error("the ConfigMap template does not render config_schema_version")
	}
}

// Every version in the additions table has to be one the binary could ask for.
// An entry above minConfigSchema names settings for a schema nothing requires,
// which is a bump somebody forgot.
func TestTheAdditionsTableStaysWithinTheSchema(t *testing.T) {
	for v, keys := range schemaAdditions {
		if v > minConfigSchema {
			t.Errorf("schemaAdditions has version %d (%v) above minConfigSchema %d", v, keys, minConfigSchema)
		}
		if v < 1 {
			t.Errorf("schemaAdditions has version %d, which no chart can render", v)
		}
	}
}

// The warning must name every setting added since the chart's schema, not only
// the ones from the newest step.
//
// A deployment two or three schemas behind is the ordinary case -- charts are
// upgraded less often than binaries -- and a warning that named only the last
// step would send an operator looking at the wrong settings.
func TestTheWarningNamesEverySettingAddedSince(t *testing.T) {
	additions := map[int][]string{
		2: {"hostname"},
		3: {"submission_hostname", "chart_version"},
		4: {"fts_worker_count"},
	}
	for _, tc := range []struct {
		name string
		have int
		want int
		out  []string
	}{
		{"one step behind", 3, 4, []string{"fts_worker_count"}},
		{"three steps behind", 1, 4, []string{"chart_version", "fts_worker_count", "hostname", "submission_hostname"}},
		{"a chart from before the schema existed", 0, 4, []string{"chart_version", "fts_worker_count", "hostname", "submission_hostname"}},
		{"current", 4, 4, nil},
		{"ahead of the binary", 5, 4, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultedByOlderSchema(tc.have, tc.want, additions)
			if len(got) != len(tc.out) {
				t.Fatalf("named %v, want %v", got, tc.out)
			}
			for i := range got {
				if got[i] != tc.out[i] {
					t.Errorf("named %v, want %v", got, tc.out)
					break
				}
			}
		})
	}
}

// codeKeys is every key the binary reads, taken from the koanf tags on the
// config struct rather than from a list somebody maintains.
func codeKeys(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`koanf:"([a-z0-9_]+)"`).FindAllSubmatch(raw, -1) {
		out[string(m[1])] = true
	}
	if len(out) < 100 {
		t.Fatalf("found only %d config keys, so this test is not looking at what it thinks", len(out))
	}
	return out
}

// chartKeys is every key configmap.yaml writes, at any nesting depth.
func chartKeys(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "configmap.yaml"))
	if err != nil {
		t.Fatalf("read configmap.yaml: %v", err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+([a-z0-9_]+):`).FindAllSubmatch(raw, -1) {
		out[string(m[1])] = true
	}
	if len(out) < 100 {
		t.Fatalf("found only %d rendered keys, so this test is not looking at what it thinks", len(out))
	}
	return out
}

// A key the binary reads is either rendered by the chart or listed as one that
// is not, with a reason.
//
// This is the test the schema version is worth having. The other three check
// that two numbers move together; this one is what notices that they should
// have. Without it the scheme is discipline, and the defect it exists for --
// a binary reading a key the chart does not render, taking its default, and
// the symptom reading as a code bug -- is caught by nobody.
func TestEveryKeyTheBinaryReadsIsRenderedOrDeclaredUnrendered(t *testing.T) {
	code, chart := codeKeys(t), chartKeys(t)

	var undeclared []string
	for k := range code {
		if chart[k] {
			continue
		}
		if _, known := notRenderedByTheChart[k]; known {
			continue
		}
		undeclared = append(undeclared, k)
	}
	sort.Strings(undeclared)
	for _, k := range undeclared {
		t.Errorf("the binary reads %q, configmap.yaml does not write it, and it is not declared in notRenderedByTheChart: "+
			"add the line to the chart and bump the schema, or declare why the default is the deployed value", k)
	}
}

// The row that will actually happen: a chart from before the schema existed.
//
// Every deployment upgraded from 2.3.x renders no config_schema_version, so the
// binary reads 0, and this is the warning it produces. Synthetic rows above
// prove the arithmetic; this one proves the shipped table is not empty.
func TestAChartFromBeforeTheSchemaIsToldWhatItIsMissing(t *testing.T) {
	got := defaultedByOlderSchema(0, minConfigSchema, schemaAdditions)
	if len(got) == 0 {
		t.Fatal("a chart at schema 0 is told nothing: the additions table names no keys, so the warning is the bare number the chart version already gave")
	}
	for _, want := range []string{"hostname", "chart_version"} {
		found := false
		for _, k := range got {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not named, though a chart that predates the schema does not render it: got %v", want, got)
		}
	}
}

// The inventory must not grow stale in the other direction: a key listed as
// unrendered that the chart now renders is a line somebody added without
// removing the exemption, and the exemption would hide the next one.
func TestNothingIsDeclaredUnrenderedWhileTheChartRendersIt(t *testing.T) {
	chart := chartKeys(t)
	for k := range notRenderedByTheChart {
		if chart[k] {
			t.Errorf("%q is declared as not rendered, but configmap.yaml writes it: remove the exemption", k)
		}
	}
}
