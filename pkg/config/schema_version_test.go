package config

import (
	"os"
	"path/filepath"
	"regexp"
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
