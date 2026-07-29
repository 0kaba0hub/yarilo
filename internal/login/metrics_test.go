package login

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPhaseLabelsAreStable(t *testing.T) {
	// The dashboards and the #881 acceptance criterion key off these exact
	// strings; a rename silently breaks every saved query.
	tests := []struct {
		name  string
		phase string
		want  string
	}{
		{"tls", phaseTLSHandshake, "tls_handshake"},
		{"preamble", phasePreamble, "preamble"},
		{"auth dial", phaseAuthDial, "auth_dial"},
		{"auth", phaseAuth, "auth"},
		{"director lookup", phaseDirectorLookup, "director_lookup"},
		{"anvil connect", phaseAnvilConnect, "anvil_connect"},
		{"backend dial", phaseBackendDial, "backend_dial"},
		{"backend preamble", phaseBackendPreamble, "backend_preamble"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.phase != tc.want {
				t.Fatalf("phase label = %q, want %q", tc.phase, tc.want)
			}
		})
	}
}

func TestObservePhaseRecordsUnderProtocolLabel(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		phase    string
	}{
		{"imap auth", ProtocolIMAP, phaseAuth},
		{"imap backend dial", ProtocolIMAP, phaseBackendDial},
		{"pop3 auth", ProtocolPOP3, phaseAuth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{opts: Options{Protocol: tc.protocol}}
			before := testutil.CollectAndCount(phaseSeconds)
			srv.observePhase(tc.phase, time.Now())
			if got := testutil.CollectAndCount(phaseSeconds); got < before {
				t.Fatalf("series count shrank: %d → %d", before, got)
			}
			out, err := gatherMetric("yarilo_login_phase_seconds")
			if err != nil {
				t.Fatal(err)
			}
			wantPhase := `phase="` + tc.phase + `"`
			wantProto := `protocol="` + string(tc.protocol) + `"`
			if !strings.Contains(out, wantPhase) || !strings.Contains(out, wantProto) {
				t.Fatalf("missing labels %s / %s in:\n%s", wantPhase, wantProto, out)
			}
		})
	}
}

func TestIncResultRecordsEveryOutcome(t *testing.T) {
	// Every result the login path can produce must be a distinct series —
	// collapsing unavailable into auth_failed was what made #878 hard to read.
	tests := []struct {
		name   string
		result string
	}{
		{"ok", "ok"},
		{"unavailable", "unavailable"},
		{"backend rejected", "backend_rejected"},
		{"preamble error", "preamble_error"},
		{"tls error", "tls_error"},
	}
	srv := &Server{opts: Options{Protocol: ProtocolIMAP}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv.incResult(tc.result)
			out, err := gatherMetric("yarilo_login_result_total")
			if err != nil {
				t.Fatal(err)
			}
			if want := `result="` + tc.result + `"`; !strings.Contains(out, want) {
				t.Fatalf("missing %s in:\n%s", want, out)
			}
		})
	}
}

// gatherMetric renders the named metric family from the default registry in the
// Prometheus text format.
func gatherMetric(name string) (string, error) {
	var sb strings.Builder
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", err
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			sb.WriteString(f.GetName())
			sb.WriteString("{")
			for _, l := range m.GetLabel() {
				sb.WriteString(l.GetName())
				sb.WriteString(`="`)
				sb.WriteString(l.GetValue())
				sb.WriteString(`" `)
			}
			sb.WriteString("}\n")
		}
	}
	return sb.String(), nil
}
