package loginproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	masterclient "github.com/yarilomail/yarilo/pkg/authclient"
)

// captureLogs installs a handler at Debug and gives the buffer back, so a test
// can assert on the LEVEL as well as the text -- which is the whole subject
// here.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "10.0.0.7:40000" }

type fakeConn struct{ net.Conn }

func (fakeConn) RemoteAddr() net.Addr { return fakeAddr{} }

// An operator running at info saw nothing while every session on a backend was
// being refused, because one Debug line covered both "a peer sent something
// unparseable" and "the dependency behind us is gone" (#1427). The two are told
// apart by the marker the auth client carries.
func TestADependencyOutageIsLoudAndTheRestIsNot(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantWarn bool
	}{
		{
			name:     "the auth master is unreachable",
			err:      fmt.Errorf("userdb lookup: %w", masterclient.ErrUnavailable),
			wantWarn: true,
		},
		{
			// Routine: a client that spoke nonsense, or a token that expired
			// while it travelled. At info this would be noise nobody acts on.
			name:     "a preamble we could not parse",
			err:      errors.New("preamble read: not a YARILO preamble"),
			wantWarn: false,
		},
		{
			name:     "a token that did not verify",
			err:      errors.New("token verify: unknown token"),
			wantWarn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLogs(t)
			l := &PreambleListener{ExpectedService: "imap"}
			l.noteHandshakeFailure(fakeConn{}, tt.err)

			out := buf.String()
			gotWarn := strings.Contains(out, "level=WARN")
			if gotWarn != tt.wantWarn {
				t.Errorf("WARN = %v, want %v -- logged: %s", gotWarn, tt.wantWarn, out)
			}
			if tt.wantWarn && !strings.Contains(out, "dependency is unreachable") {
				t.Errorf("the loud line does not say what happened: %s", out)
			}
			if out == "" {
				t.Error("nothing was logged at all")
			}
		})
	}
}

// The record must carry each attribute once. pkg/logging puts "service" on
// every record of the process, and this call site added a second copy: a
// duplicate JSON key is legal to write and undefined to read -- encoding/json
// keeps the last, strict parsers reject the whole object, and log pipelines
// index whichever they happened to see (#1429). For the field naming the
// service, that ambiguity is the worst place to have it.
//
// Asserted on the JSON handler with the process attribute attached, because
// that is the shape a deployment emits; the text handler used elsewhere in
// this file would show the duplicate too, but not the object a parser chokes
// on.
func TestTheWarningCarriesEachAttributeOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})).With("service", "imap"))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := &PreambleListener{ExpectedService: "imap"}
	l.noteHandshakeFailure(fakeConn{}, fmt.Errorf("userdb lookup: %w", masterclient.ErrUnavailable))

	line := buf.String()
	if got := strings.Count(line, `"service"`); got != 1 {
		t.Errorf(`"service" appears %d times in one record, want 1: %s`, got, line)
	}
	// And it survives a strict reader: a duplicate key is exactly what makes
	// this fail for some consumers and not others.
	var into map[string]any
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&into); err != nil {
		t.Fatalf("the record does not parse: %v -- %s", err, line)
	}
	if into["service"] != "imap" {
		t.Errorf("service = %v, want imap", into["service"])
	}
}

// A dependency that is down refuses every session that arrives. One line per
// session would bury the log exactly when somebody is reading it, so the loud
// line is throttled -- and the quiet one still records every occurrence, so a
// debug run loses nothing.
func TestTheLoudLineIsThrottledAndNothingIsLost(t *testing.T) {
	buf := captureLogs(t)
	l := &PreambleListener{ExpectedService: "imap"}
	outage := fmt.Errorf("userdb lookup: %w", masterclient.ErrUnavailable)

	const attempts = 5
	for i := 0; i < attempts; i++ {
		l.noteHandshakeFailure(fakeConn{}, outage)
	}

	out := buf.String()
	if got := strings.Count(out, "level=WARN"); got != 1 {
		t.Errorf("WARN lines = %d for %d refusals, want 1", got, attempts)
	}
	// The rest are still there at debug: throttling the loud line must not
	// throw away the record of what happened.
	if got := strings.Count(out, "preamble handshake failed"); got != attempts-1 {
		t.Errorf("debug lines = %d, want %d -- the throttled occurrences were dropped", got, attempts-1)
	}

	// Once the interval has passed, the outage is loud again: an outage that
	// lasts an hour must not be announced only in its first minute.
	l.depLast = time.Now().Add(-2 * dependencyFailureLogEvery)
	l.noteHandshakeFailure(fakeConn{}, outage)
	if got := strings.Count(buf.String(), "level=WARN"); got != 2 {
		t.Errorf("WARN lines = %d after the interval elapsed, want 2", got)
	}
}
