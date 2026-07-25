package telemetry

import (
	"os"
	"testing"
)

func TestAddr(t *testing.T) {
	t.Setenv("TELEMETRY_LISTEN", "")
	os.Unsetenv("TELEMETRY_LISTEN")
	if got := Addr(":9000"); got != ":9000" {
		t.Errorf("unset env: got %q, want :9000", got)
	}
	if got := Addr(""); got != ":8080" {
		t.Errorf("unset env + empty cfg: got %q, want :8080", got)
	}
	t.Setenv("TELEMETRY_LISTEN", ":8083")
	if got := Addr(":9000"); got != ":8083" {
		t.Errorf("env override: got %q, want :8083", got)
	}
}
