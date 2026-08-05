package ftsservice

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Where FTS data goes was derived and had no way to be stated. The two kinds of
// data have different requirements — FTS is derived and write-heavy, mail is
// neither — and a deployment could not place them accordingly (#1053).
func TestIndexRoot(t *testing.T) {
	info := &mailbox.UserInfo{
		Username: "alice@example.com",
		Home:     "/var/mail/example.com/alice",
		MailPath: "/var/mail/example.com/alice/Maildir",
		IndexDir: "/var/index/example.com/alice",
	}

	for _, tc := range []struct {
		name       string
		configured string
		info       *mailbox.UserInfo
		want       string
	}{
		// Unset keeps every step of the old derivation, in order.
		{"unset falls back to the index dir", "", info, "/var/index/example.com/alice"},
		{"unset falls back to the mail path", "",
			&mailbox.UserInfo{Home: "/home/u", MailPath: "/home/u/Maildir"}, "/home/u/Maildir"},
		{"unset falls back to the home", "",
			&mailbox.UserInfo{Home: "/home/u"}, "/home/u"},

		// Configured wins over all of it, including an explicit index dir —
		// otherwise a deployment that sets both gets the one it did not choose.
		{"configured wins", "/var/fts", info, "/var/fts"},

		// Expanded like every other storage location.
		{"expands the home marker", "%h/fts", info, "/var/mail/example.com/alice/fts"},
		{"expands the tilde", "~/fts", info, "/var/mail/example.com/alice/fts"},
		{"expands the user", "/var/fts/%u", info, "/var/fts/alice@example.com"},
		{"expands local and domain", "/var/fts/%d/%n", info, "/var/fts/example.com/alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{opts: Options{IndexRoot: tc.configured}}
			if got := s.indexRoot(tc.info); got != tc.want {
				t.Errorf("indexRoot = %q, want %q", got, tc.want)
			}
		})
	}
}

// Two users must not share a root, or one account's index is written over
// another's. The expansion is what keeps them apart.
func TestIndexRootSeparatesUsers(t *testing.T) {
	s := &Service{opts: Options{IndexRoot: "/var/fts/%d/%n"}}
	a := s.indexRoot(&mailbox.UserInfo{Username: "alice@example.com", Home: "/h/a"})
	b := s.indexRoot(&mailbox.UserInfo{Username: "bob@example.com", Home: "/h/b"})
	if a == b {
		t.Fatalf("both users resolved to %q", a)
	}
}

// A root naming no user is refused at startup rather than served.
//
// The per-folder subpath below the root separates mailboxes, not accounts, so
// such a template puts two people's INBOX index at one path. That is not
// over-indexing, which heals; it is two accounts writing the same files, and
// the only answer that does not corrupt something quietly is to refuse.
func TestIndexRootWithoutAUserVariableIsRefused(t *testing.T) {
	for _, tc := range []struct {
		tmpl     string
		accepted bool
	}{
		{"", true}, // unset keeps the derived path
		{"%h/fts", true},
		{"~/fts", true},
		{"/var/fts/%u", true},
		{"/var/fts/%d/%n", true},

		{"/var/fts", false},
		{"/srv/index", false},
	} {
		t.Run(tc.tmpl, func(t *testing.T) {
			err := checkIndexRoot(tc.tmpl)
			if accepted := err == nil; accepted != tc.accepted {
				t.Errorf("checkIndexRoot(%q) error = %v, want accepted=%v", tc.tmpl, err, tc.accepted)
			}
			if !tc.accepted && err != nil && !strings.Contains(err.Error(), "share one index directory") {
				t.Errorf("the error does not say what goes wrong: %v", err)
			}
		})
	}
}
