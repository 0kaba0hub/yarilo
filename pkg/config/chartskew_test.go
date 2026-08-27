package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/build"
)

// The ConfigMap and the binary must come from one chart version, and when they
// do not somebody has to be told.
//
// Nothing said so: `helm upgrade --set image.tag=X` deploys a new image with
// whatever chart the working copy holds, and a gate run got a binary reading a
// key the chart in that checkout did not render. The key was simply absent, the
// default was used, and the symptom read as the code taking the wrong knob
// (#1509).
//
// The absent-key row is the one that matters: that is the shape the trap
// actually had, and a check that only compared two present strings would have
// missed it.
func TestChartSkewIsReported(t *testing.T) {
	tests := []struct {
		name       string
		builtFrom  string
		fromConfig string
		wantWarn   string
	}{
		{
			name:      "a local build is not deployed from a chart",
			builtFrom: "dev", fromConfig: "",
		},
		{
			name:      "one commit",
			builtFrom: "2.3.266", fromConfig: "2.3.266",
		},
		{
			name:      "the chart is older than the binary and renders no version at all",
			builtFrom: "2.3.266", fromConfig: "",
			wantWarn: "no chart_version",
		},
		{
			name:      "two chart versions",
			builtFrom: "2.4.1", fromConfig: "2.3.266",
			wantWarn: "rendered by one chart version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevBuild := build.ChartVersion
			build.ChartVersion = tt.builtFrom
			defer func() { build.ChartVersion = prevBuild }()

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			warnChartSkew(tt.fromConfig)

			switch {
			case tt.wantWarn == "" && buf.Len() > 0:
				t.Errorf("warned about a pairing that is fine:\n%s", buf.String())
			case tt.wantWarn != "" && !strings.Contains(buf.String(), tt.wantWarn):
				t.Errorf("warning does not say %q:\n%s", tt.wantWarn, buf.String())
			}
		})
	}
}
