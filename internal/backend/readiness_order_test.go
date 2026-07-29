package backend

import (
	"net"
	"strings"
	"testing"
)

// TestBindTCPHoldsThePortBeforeReturning is the property the Run* ordering relies
// on: once bind returns, the port is accepting. Readiness is reported after these
// calls, so a client that arrives the instant Kubernetes adds the pod to a Service
// must find the port open rather than getting connection refused (#899).
func TestBindTCPHoldsThePortBeforeReturning(t *testing.T) {
	ln, err := bindTCP("test", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bindTCP: %v", err)
	}
	defer ln.Close()

	// Connectable immediately, with nothing accepting yet — which is exactly the
	// window that used to exist between SetReady and the serve goroutine binding.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("port not accepting right after bind: %v", err)
	}
	c.Close()
}

// TestBindTCPFailsLoudlyOnAPortInUse: a bind failure must be an error the caller
// returns, not a log line inside a goroutine. Before this change the listen
// happened after readiness was announced, so a taken port produced a pod that
// reported ready and served nothing.
func TestBindTCPFailsLoudlyOnAPortInUse(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer first.Close()

	if _, err := bindTCP("test", first.Addr().String()); err == nil {
		t.Fatal("binding an occupied port must fail")
	}
}

func TestBindTLSRequiresAConfig(t *testing.T) {
	// A nil TLS config would otherwise produce a listener that fails every
	// handshake, i.e. a port that accepts and then rejects everyone — worse than
	// refusing to start.
	ln, err := bindTLS("test", "127.0.0.1:0", nil)
	if err == nil {
		ln.Close()
		t.Fatal("bindTLS with a nil config must fail")
	}
}

func TestBindTCPErrorNamesProtocolAndAddress(t *testing.T) {
	// The error goes into a startup failure, where naming both is what makes it
	// diagnosable from a CrashLoopBackOff.
	_, err := bindTCP("imap", "256.256.256.256:99999")
	if err == nil {
		t.Fatal("expected an error for an invalid address")
	}
	msg := err.Error()
	for _, want := range []string{"imap", "256.256.256.256"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}
