package mailbox_test

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// One entry per identifier and sign, decided at parse.
//
// Duplicates were legal on disk and unioned at evaluation, so a write path that
// appended instead of replacing looked like it worked: grant "lr", then "sk",
// and the mailbox resolves "lrsk". An attempt to reduce resolved to the old,
// wider value, which is why an admin ACL could only ever widen (#1114).
func TestParseACLCollapsesDuplicateIdentifiers(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  string
		count int
	}{
		{
			// The later line is the statement that counts, so an appended
			// grant does not add to the earlier one.
			name:  "appended grant",
			body:  "user=u2 lr\nuser=u2 sk\n",
			want:  "user=u2 sk\n",
			count: 1,
		},
		{
			// The reduction attempt from the issue: the file ends up saying
			// what the last write meant, not the union of every write.
			name:  "reduction attempt",
			body:  "user=u2 lrskxa\nuser=u2 lr\n",
			want:  "user=u2 lr\n",
			count: 1,
		},
		{
			name:  "positive and negative stay apart",
			body:  "user=u2 lr\n-user=u2 s\n",
			want:  "user=u2 lr\n-user=u2 s\n",
			count: 2,
		},
		{
			name:  "duplicates on the negative side too",
			body:  "-user=u2 s\n-user=u2 k\n",
			want:  "-user=u2 k\n",
			count: 1,
		},
		{
			name:  "different identifiers are untouched",
			body:  "user=u2 lr\nanyone l\n",
			want:  "user=u2 lr\nanyone l\n",
			count: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl, err := mailbox.ParseACLString(tc.body)
			if err != nil {
				t.Fatalf("ParseACLString: %v", err)
			}
			if len(acl) != tc.count {
				t.Errorf("entries = %d, want %d: %q", len(acl), tc.count, acl.String())
			}
			if got := acl.String(); got != tc.want {
				t.Errorf("collapsed to %q, want %q", got, tc.want)
			}
		})
	}
}

// The structural half: with one entry per identifier and sign, an identifier's
// two masks are decided by one entry each, so which of the two lines comes
// first in the file cannot change the answer.
//
// (Two lines of the *same* sign are order-dependent on purpose — the later is
// the current statement. That is the point of collapsing, not a side effect.)
func TestPositiveAndNegativeOrderDoesNotMatter(t *testing.T) {
	forward, err := mailbox.ParseACLString("user=u2 lrs\n-user=u2 s\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reverse, err := mailbox.ParseACLString("-user=u2 s\nuser=u2 lrs\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	gotF, gotR := forward.Effective("u2", nil, false), reverse.Effective("u2", nil, false)
	if gotF != gotR {
		t.Errorf("same ACL, different line order: %q vs %q", gotF, gotR)
	}
	if gotF != "lr" {
		t.Errorf("effective = %q, want lr — the negative removes s", gotF)
	}
}

// Collapsing is idempotent: a file already normalised is unchanged, which is
// what lets it run on every read and every write without the two disagreeing.
func TestCollapseIsIdempotent(t *testing.T) {
	acl, err := mailbox.ParseACLString("user=u2 lr\n-user=u2 s\nanyone l\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := acl.Collapse().String(); got != acl.String() {
		t.Errorf("second collapse changed %q to %q", acl.String(), got)
	}
}
