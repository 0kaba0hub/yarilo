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

// TestProbeTCPReachable: a dependency with no HTTP endpoint (a database or Redis)
// is probed by opening a TCP connection — a live listener passes (#903).
func TestProbeTCPReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Accept and drop connections so the dial completes.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	if err := probeURL("tcp://"+ln.Addr().String(), time.Second); err != nil {
		t.Fatalf("a live TCP listener must pass: %v", err)
	}
}

// TestProbeTCPUnreachable: nothing listening behind the address fails the probe,
// and the error names the tcp:// target.
func TestProbeTCPUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port

	err = probeURL("tcp://"+addr, 500*time.Millisecond)
	if err == nil {
		t.Fatal("an unreachable TCP address must fail the probe")
	}
	if !strings.Contains(err.Error(), "tcp://"+addr) {
		t.Fatalf("error %q should name the tcp:// target", err)
	}
}

func TestDispatchWaitRequiresAURL(t *testing.T) {
	if err := dispatchWait(nil); err == nil {
		t.Fatal("wait without a URL must be an error, not a silent success")
	}
}

// TestWaitURLsSurviveGlobalFlagExtraction is the regression test for the bug this
// subcommand shipped with: yarctl registers a GLOBAL --url (the Director API base
// URL) and pulls global flags out of argv before any subcommand sees them, so
// `yarctl wait --url=...` lost its URL and the probe failed with "at least one URL
// is required" whatever was passed. It reached the cluster because the original
// test called dispatchWait directly and skipped extractGlobalFlags entirely.
//
// Asserting on the real argv path is the point: URLs must arrive as positional
// arguments, untouched by the extractor.
func TestWaitURLsSurviveGlobalFlagExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	argv := []string{"wait", "--timeout=1s", srv.URL}
	globals, rest := extractGlobalFlags(argv)

	// The URL must not have been captured as a global flag value.
	for _, g := range globals {
		if g == srv.URL {
			t.Fatalf("the probe URL was swallowed by global flag extraction: globals=%v", globals)
		}
	}
	if len(rest) == 0 || rest[0] != "wait" {
		t.Fatalf("dispatch tokens lost: rest=%v", rest)
	}
	if err := dispatchWait(rest[1:]); err != nil {
		t.Fatalf("wait via the real argv path: %v", err)
	}
}

// TestWaitRejectsTheOldFlagForm guards the chart against silently regressing to
// --url, which would parse as the global flag and leave the probe permanently
// failing rather than erroring visibly.
func TestWaitRejectsTheOldFlagForm(t *testing.T) {
	if err := dispatchWait([]string{"--url=http://example.invalid/readyz"}); err == nil {
		t.Fatal("--url must be rejected outright, not accepted as a URL")
	}
}

// TestDispatchWaitAllURLsMustPass: a startupProbe listing several dependencies
// must not pass when only some of them are ready.
func TestDispatchWaitAllURLsMustPass(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	if err := dispatchWait([]string{"--timeout", "1s", ok.URL, ok.URL}); err != nil {
		t.Fatalf("two ready dependencies should pass: %v", err)
	}
	// The failing case calls os.Exit so kubelet sees a non-zero status, which
	// cannot be asserted in-process; probeURL covers the status taxonomy.
}
