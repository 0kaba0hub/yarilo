package config

import (
	"bytes"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The retired key must be answered on its PRESENCE, not on its value.
//
// It was answered on the value, so `telemetry_pprof_heap_enabled: false` --
// what the documentation told operators to write, and what the sandbox ran on
// -- warned nobody, and the dead key stayed in the file forever. The row that
// matters here is "false", which the old check passed by staying silent
// (#1493).
func TestARetiredKeyIsAnsweredOnPresenceNotValue(t *testing.T) {
	tests := []struct {
		name string
		body string
		warn bool
	}{
		{
			name: "set to true",
			body: "telemetry:\n  telemetry_pprof_heap_enabled: true\n",
			warn: true,
		},
		{
			name: "set to false — the ordinary value, and the one that used to say nothing",
			body: "telemetry:\n  telemetry_pprof_heap_enabled: false\n",
			warn: true,
		},
		{
			name: "absent",
			body: "telemetry:\n  telemetry_pprof_enabled: true\n",
			warn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			if _, err := loadYAML(t, tt.body); err != nil {
				t.Fatalf("load: %v", err)
			}
			got := strings.Contains(buf.String(), "telemetry_pprof_heap_enabled")
			if got != tt.warn {
				t.Errorf("warned = %v, want %v; log was:\n%s", got, tt.warn, buf.String())
			}
		})
	}
}

// A retired key listed while the chart still writes it warns on every install,
// permanently -- and a warning everyone sees on a healthy deployment is one
// nobody reads on a sick one. So the chart must not carry any of them.
//
// Checked here rather than in the chart tests because this is the list's own
// precondition: the guard has to be able to see what is on the list.
func TestNoRetiredKeyIsStillWrittenByTheChart(t *testing.T) {
	chart := readChartText(t)
	for _, r := range retiredKeys() {
		if strings.Contains(chart, path.Base(strings.ReplaceAll(r.key, ".", "/"))) {
			t.Errorf("%s is retired but the chart still writes it; every install would carry the key and every start would warn", r.key)
		}
	}
	if len(retiredKeys()) == 0 {
		t.Skip("no retired keys to check")
	}
}

// readChartText returns values.yaml and every template as one string. Textual
// rather than a helm render, for the same reason the chart guard in
// internal/helmchart is: it needs no helm binary, so it runs wherever the unit
// tests do.
func readChartText(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	root := filepath.Join("..", "..", "helm")
	paths, err := filepath.Glob(filepath.Join(root, "templates", "*"))
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	paths = append(paths, filepath.Join(root, "values.yaml"))
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		b.Write(body)
	}
	if b.Len() < 1000 {
		t.Fatalf("read only %d bytes of chart; the search is no longer finding it", b.Len())
	}
	return b.String()
}
