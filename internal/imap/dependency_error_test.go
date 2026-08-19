package imap

import (
	"errors"
	"fmt"
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
