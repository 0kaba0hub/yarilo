package imap

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// SERVERBUG says "this server is broken, the request will never work", and a
// client that gets it stops retrying and shows the user something they cannot
// act on. A lock service being redeployed is the opposite: the same request
// succeeds seconds later, which is what UNAVAILABLE means (RFC 5530, #1339).
//
// The rows that matter are the two ends: a dependency outage is reclassified,
// and anything else is left exactly as it was -- a helper that rewrote every
// error would hide real bugs behind "try again".
func TestDependencyErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode imaplib.ResponseCode
		wantSame bool
	}{
		{
			name:     "a lock service that cannot be reached",
			err:      fmt.Errorf("locks/client: read: %w: %w", locks.ErrUnavailable, errors.New("EOF")),
			wantCode: imaplib.ResponseCodeUnavailable,
		},
		{
			name:     "wrapped deeper, as a storage layer would",
			err:      fmt.Errorf("fileindex: write flags: %w", fmt.Errorf("locks: %w", locks.ErrUnavailable)),
			wantCode: imaplib.ResponseCodeUnavailable,
		},
		{
			// The lock service answering "busy" is not an outage: the resource
			// is held, which is a different thing to tell a client.
			name:     "a busy resource is not an outage",
			err:      fmt.Errorf("locks: %w", locks.ErrBusy),
			wantSame: true,
		},
		{
			name:     "an ordinary failure is untouched",
			err:      errors.New("index corrupt"),
			wantSame: true,
		},
		{
			name:     "nil stays nil",
			err:      nil,
			wantSame: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dependencyError(tc.err)
			if tc.wantSame {
				if got != nil && !errors.Is(got, tc.err) {
					t.Errorf("error was rewritten: %v -> %v", tc.err, got)
				}
				return
			}
			var imapErr *imaplib.Error
			if !errors.As(got, &imapErr) {
				t.Fatalf("got %T, want an *imap.Error -- anything else becomes SERVERBUG in the library", got)
			}
			if imapErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", imapErr.Code, tc.wantCode)
			}
		})
	}
}

// The ACL answer carries the same classification, so a client sees one story
// for one outage whichever command it happened to run.
func TestACLUnavailableUsesTheTemporaryCode(t *testing.T) {
	outage := fmt.Errorf("userstate/acl: %w", locks.ErrUnavailable)
	var imapErr *imaplib.Error
	if !errors.As(aclUnavailable("INBOX", outage), &imapErr) {
		t.Fatal("the ACL failure is not an *imap.Error")
	}
	if imapErr.Code != imaplib.ResponseCodeUnavailable {
		t.Errorf("code = %q, want UNAVAILABLE", imapErr.Code)
	}
	// A parse error in an ACL file is a real defect, and must not be dressed
	// up as something to retry.
	var other *imaplib.Error
	if !errors.As(aclUnavailable("INBOX", errors.New("bad acl line 3")), &other) {
		t.Fatal("not an *imap.Error")
	}
	if other.Code == imaplib.ResponseCodeUnavailable {
		t.Error("a malformed ACL file was reported as a temporary outage")
	}
	// And the text must agree with the code: a permanent code beside "try
	// again" tells the client two different things, and a client believes the
	// code.
	if strings.Contains(strings.ToLower(other.Text), "try again") {
		t.Errorf("code %q is paired with text %q", other.Code, other.Text)
	}
	if !strings.Contains(strings.ToLower(imapErr.Text), "try again") {
		t.Errorf("the temporary answer %q does not tell the client to retry", imapErr.Text)
	}
}

// The seam, not the call site. STORE was classified at one call and the failure
// came from another in the same handler, so a lock-service restart still
// reached clients as SERVERBUG (#1339). Every command now passes through
// timedSession on its way back, so the classification cannot be one site
// behind -- and a command added later cannot miss it, because the interface
// assertion in that file stops the build until it is forwarded.
func TestEverySessionMethodClassifiesThroughTheSeam(t *testing.T) {
	body, err := os.ReadFile("timed_session.go")
	if err != nil {
		t.Fatalf("read seam: %v", err)
	}
	src := string(body)

	// Every forwarded call either classifies, or is one of the named
	// exceptions. The first three answer no client request of their own; ID
	// answers one but returns no error to classify -- its answer is
	// configuration, so there is no store call behind it that can fail.
	exempt := map[string]bool{"SessionID": true, "Close": true, "AuthenticateMechanisms": true, "ID": true}

	// Both shapes are checked, because only checking the one-liner left the
	// common shape unguarded: a call that takes its error into a variable and
	// then returns it plainly passed this test while reaching clients as
	// SERVERBUG. Found by mutating a site the guard was supposed to protect.
	lines := strings.Split(src, "\n")
	var unclassified []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		oneLiner := strings.HasPrefix(trimmed, "return t.s.")
		captured := strings.HasPrefix(trimmed, "v, err := t.s.") || strings.HasPrefix(trimmed, "err := t.s.") ||
			strings.HasPrefix(trimmed, "err = t.s.")
		if !oneLiner && !captured {
			continue
		}
		call := trimmed
		for _, prefix := range []string{"return ", "v, err := ", "err := ", "err = "} {
			call = strings.TrimPrefix(call, prefix)
		}
		name := call[len("t.s."):]
		if i := strings.Index(name, "("); i > 0 {
			name = name[:i]
		}
		if exempt[name] {
			continue
		}
		if oneLiner {
			unclassified = append(unclassified, name)
			continue
		}
		// The error was captured: the return that follows must hand it to the
		// classifier rather than pass it through.
		classified := false
		for _, next := range lines[i+1:] {
			nt := strings.TrimSpace(next)
			if !strings.HasPrefix(nt, "return ") {
				if nt == "}" {
					break
				}
				continue
			}
			classified = strings.Contains(nt, "t.classify(err)")
			break
		}
		if !classified {
			unclassified = append(unclassified, name)
		}
	}
	// And the seam must reach the classifier through t.classify, never through
	// the pure mapper: the mapper cannot log, because it knows neither the
	// account nor the folder. Calling it directly returns the right answer to
	// the client and writes nothing for the operator -- which is the half of
	// #1344 that was worth more than the code.
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "dependencyError(") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "out := dependencyError(err)") {
			continue // classify itself
		}
		t.Errorf("timed_session.go:%d calls dependencyError directly: %s\n  use t.classify, or the operator gets no line for a failure the client is told about",
			i+1, trimmed)
	}

	if len(unclassified) > 0 {
		t.Errorf("these commands return the store's error unclassified, so the library turns it into SERVERBUG:\n  %s",
			strings.Join(unclassified, "\n  "))
	}
}
