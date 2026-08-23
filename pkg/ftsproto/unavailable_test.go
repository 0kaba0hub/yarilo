package ftsproto

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// failingService fails every call with a chosen error, so the tests below
// exercise the whole path a real failure takes: classified in the service,
// written to the wire, parsed by the client. A sentinel that never crossed
// that boundary is what #1409 was about.
type failingService struct{ err error }

func (f failingService) Index(string, fts.MailboxRef, uint32, int) error { return f.err }
func (f failingService) Prepend(string, fts.MailboxRef, uint32) error    { return f.err }
func (f failingService) Expunge(string, fts.MailboxRef, uint32) error    { return f.err }
func (f failingService) Rescan(string, fts.MailboxRef) error             { return f.err }
func (f failingService) Optimize(string) error                           { return f.err }
func (f failingService) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, f.err
}
func (f failingService) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	return 0, 0, f.err
}

func serveFailing(t *testing.T, err error) *Remote {
	t.Helper()
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	go Serve(ln, failingService{err: err}) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	r, derr := Dial(ln.Addr().String(), 5*time.Second)
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(func() { r.Close() }) //nolint:errcheck
	return r
}

var testMbox = fts.MailboxRef{Name: "INBOX", GUID: "g1", UIDValidity: 1}

func TestUnavailabilitySurvivesTheWire(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "auth unreachable inside the fts service",
			err:  fmt.Errorf("ftsservice: userdb u@example.com: %w", authclient.ErrUnavailable),
			want: true,
		},
		{
			name: "the lock service unreachable",
			err:  fmt.Errorf("fts: index write: %w", locks.ErrUnavailable),
			want: true,
		},
		{
			// A broken index is not an outage: retrying returns the same
			// thing, and telling a client to try again would be a lie it acts
			// on.
			name: "an ordinary failure",
			err:  errors.New("flatcurve: shard is corrupt"),
			want: false,
		},
		{
			// The negative row that the coded form makes necessary: error text
			// that itself begins with the code word must not be read as one.
			// It arrives as a single field, because the builder flattens it.
			name: "error text that starts with the code word",
			err:  errors.New("UNAVAILABLE is a word this error happens to start with"),
			want: false,
		},
		{
			// The forgery the coded form makes possible: error text whose own
			// first field is the code word. Unflattened, this lands in exactly
			// the position the reader reads a code from, and an ordinary
			// failure is answered as an outage.
			//
			// The earlier version of this row put the tab in the middle, which
			// forged nothing and passed with the flattening removed -- it
			// measured the reader's first-field rule, not the builder.
			name: "error text whose first field is the code word",
			err:  errors.New(CodeUnavailable + "\tforged"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := serveFailing(t, tt.err)
			_, err := r.Lookup("u@example.com", testMbox, fts.Query{})
			if err == nil {
				t.Fatal("a failing service answered without an error")
			}
			if got := errors.Is(err, ErrUnavailable); got != tt.want {
				t.Errorf("errors.Is(err, ErrUnavailable) = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

// The status path carries it too: SEARCH reaches the service through STATUS
// and PREPEND before it ever reaches LOOKUP, and an outage during those is the
// same outage.
func TestUnavailabilityOnTheStatusPath(t *testing.T) {
	r := serveFailing(t, fmt.Errorf("ftsservice: userdb: %w", authclient.ErrUnavailable))
	if _, _, err := r.Status("u@example.com", testMbox); !errors.Is(err, ErrUnavailable) {
		t.Errorf("STATUS did not carry the outage: %v", err)
	}
	if err := r.Prepend("u@example.com", testMbox, 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("PREPEND did not carry the outage: %v", err)
	}
}

// Tolerance in both directions, which is what lets this ship without ordering
// readers before writers.
func TestTheReaderToleratesBothShapes(t *testing.T) {
	tests := []struct {
		name     string
		rest     string
		wantMark bool
		wantText string
	}{
		{
			// The older shape, from a server that predates the code.
			name:     "no code at all",
			rest:     "flatcurve: shard is corrupt",
			wantText: "flatcurve: shard is corrupt",
		},
		{
			name:     "the code we know",
			rest:     CodeUnavailable + "\tuserdb unreachable",
			wantMark: true,
			wantText: "userdb unreachable",
		},
		{
			// A code from a future version. Text, not a parse error: a reader
			// that rejected what it had not been taught would break on the
			// next code added, which is the failure this form avoids.
			name:     "a code this reader does not know",
			rest:     "OVERLOADED\tcome back later",
			wantText: "OVERLOADED\tcome back later",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refusalError(tt.rest)
			if got := errors.Is(err, ErrUnavailable); got != tt.wantMark {
				t.Errorf("marked = %v, want %v", got, tt.wantMark)
			}
			if want := "ftsproto: server: " + tt.wantText; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to carry %q", err.Error(), want)
			}
		})
	}
}
