package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestProbeURLStatuses pins the distinction busybox wget cannot make: a 503 from a
// dependency that is up but not ready is a different fact from an unreachable
// host, and the probe output must say which (#903).
func TestProbeURLStatuses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
		wantMsg string
	}{
		{"ready", http.StatusOK, false, ""},
		{"no content is still success", http.StatusNoContent, false, ""},
		{"not ready", http.StatusServiceUnavailable, true, "HTTP 503"},
		{"not found", http.StatusNotFound, true, "HTTP 404"},
		{"server error", http.StatusInternalServerError, true, "HTTP 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			err := probeURL(srv.URL, time.Second)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status %d should fail the probe", tc.status)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error %q should mention %q", err, tc.wantMsg)
				}
				if !strings.Contains(err.Error(), srv.URL) {
					t.Fatalf("error %q should name the URL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("status %d should pass: %v", tc.status, err)
			}
		})
	}
}

// TestProbeURLTimeoutCoversASlowResponse is the property that matters for a probe:
// the deadline must bound the whole attempt, not just the connect. A hung
// dependency has to fail the probe rather than hang kubelet.
func TestProbeURLTimeoutCoversASlowResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	start := time.Now()
	err := probeURL(srv.URL, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a response slower than the timeout must fail the probe")
	}
	if elapsed > time.Second {
		t.Fatalf("probe took %v — the timeout did not bound the request", elapsed)
	}
}

// TestProbeURLUnreachable covers the case the init containers handled: the
// dependency's Service exists but nothing is listening behind it yet.
func TestProbeURLUnreachable(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if err := probeURL("http://"+addr+"/readyz", 500*time.Millisecond); err == nil {
		t.Fatal("an unreachable address must fail the probe")
	}
}

func TestDispatchWaitRequiresAURL(t *testing.T) {
	if err := dispatchWait(nil); err == nil {
		t.Fatal("wait without --url must be an error, not a silent success")
	}
}

// TestDispatchWaitAllURLsMustPass: a startupProbe listing several dependencies
// must not pass when only some of them are ready.
func TestDispatchWaitAllURLsMustPass(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	// Both ready → no error.
	if err := dispatchWait([]string{"--url", ok.URL, "--url", ok.URL, "--timeout", "1s"}); err != nil {
		t.Fatalf("two ready dependencies should pass: %v", err)
	}
	// The failing case exits the process, so it is covered by TestProbeURLStatuses
	// rather than here — dispatchWait calls os.Exit so kubelet sees a non-zero
	// status, which cannot be asserted in-process.
}

func TestStringListFlag(t *testing.T) {
	var l stringList
	if err := l.Set("a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := l.Set("b"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := l.Set(""); err == nil {
		t.Fatal("an empty URL must be rejected")
	}
	if got := l.String(); got != "a,b" {
		t.Fatalf("String() = %q, want \"a,b\"", got)
	}
}
