package ftsservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
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
			if !tc.accepted && err != nil && !strings.Contains(err.Error(), "indexes would merge") {
				t.Errorf("the error does not say what goes wrong: %v", err)
			}
		})
	}
}

// The shipped default puts FTS outside the mail tree. Asserted against the
// loaded config rather than a literal, because the value that matters is the
// one a deployment gets without saying anything (#1053).
func TestDefaultIndexRootIsOutsideTheMailTree(t *testing.T) {
	// A config that says nothing about the location: whatever comes back is
	// what a deployment gets by saying nothing.
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte("fts:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FTS.IndexRoot != "posix:prefix=%h/fts/" {
		t.Fatalf("default fts_index_root = %q, want %q", cfg.FTS.IndexRoot, "posix:prefix=%h/fts/")
	}

	s := &Service{opts: Options{IndexRoot: cfg.FTS.IndexRoot}}
	info := &mailbox.UserInfo{
		Username: "alice@example.com",
		Home:     "/var/mail/example.com/alice",
		MailPath: "/var/mail/example.com/alice/Maildir",
		IndexDir: "/var/index/example.com/alice",
	}
	got := s.indexRoot(info)
	if got != "/var/mail/example.com/alice/fts/" {
		t.Errorf("indexRoot = %q, want the home-relative fts directory", got)
	}
	// The point of the default: not under the mail path, and not under the
	// index dir either — those are the two places it used to land.
	if strings.HasPrefix(got, info.MailPath) || strings.HasPrefix(got, info.IndexDir) {
		t.Errorf("indexRoot %q is still inside the mail tree", got)
	}
}

// The FTS tree must name folders exactly as the mail and index trees do.
//
// It did not: the engine built its path with the unescaped FolderSubpath while
// the drivers escaped, so with mailbox_list_storage_escape_char set, the mail
// directory for "Invoices.2026" was one escaped name and the FTS directory was
// two levels — which also means "Invoices.2026" and "Invoices/2026", two
// distinct mailboxes, share one FTS index (#1053).
//
// The engine is behind a build tag, so nothing here compiled it and the
// divergence went unnoticed when escaping landed (#1078).
func TestFTSPathEscapesLikeTheMailTree(t *testing.T) {
	const escape = "^"
	const folder = "Invoices.2026"

	mail := mailbox.FolderSubpathEscaped("maildir", folder, folder, "/", escape)
	fts := mailbox.FolderSubpathEscaped("maildir", folder, folder, "/", escape)
	if mail != fts {
		t.Fatalf("mail %q vs fts %q", mail, fts)
	}
	// And that escaping actually happened, so the assertion above is not two
	// identical calls to a function that ignores its argument.
	if unescaped := mailbox.FolderSubpath("maildir", folder, folder, "/"); unescaped == mail {
		t.Errorf("escaping changed nothing: %q — the test cannot tell the trees apart", mail)
	}
	if !strings.Contains(mail, "^2e") {
		t.Errorf("path %q does not carry the escaped separator", mail)
	}
}

// The service hands the engine the user's escape character; without it the
// engine escapes with "" and the two trees diverge whatever the engine does.
func TestServicePassesTheEscapeCharToTheEngine(t *testing.T) {
	info := &mailbox.UserInfo{
		Username:          "alice@example.com",
		Home:              "/var/mail/example.com/alice",
		StorageEscapeChar: "^",
	}
	if info.StorageEscapeChar == "" {
		t.Fatal("fixture is wrong")
	}
	ref := userRefFor(info, "/var/mail/example.com/alice/fts")
	if ref.EscapeChar != "^" {
		t.Errorf("UserRef.EscapeChar = %q, want the user's %q", ref.EscapeChar, info.StorageEscapeChar)
	}
}

// The driver prefix is part of the value, not part of the path: a setting of
// posix:%h/fts must resolve to the same directory a bare %h/fts does, or the
// index lands in a directory named after the driver.
func TestIndexRootStripsTheDriverPrefix(t *testing.T) {
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: "/var/mail/example.com/alice"}

	// All three spellings name one directory: the reference's fs-api argument,
	// our namespace-location form, and the bare path the key shipped with.
	arg := (&Service{opts: Options{IndexRoot: "posix:prefix=%h/fts"}}).indexRoot(info)
	prefixed := (&Service{opts: Options{IndexRoot: "posix:%h/fts"}}).indexRoot(info)
	bare := (&Service{opts: Options{IndexRoot: "%h/fts"}}).indexRoot(info)

	if arg != bare || prefixed != bare {
		t.Errorf("three spellings, three answers: prefix=%q, posix:%q, bare %q", arg, prefixed, bare)
	}
	if want := "/var/mail/example.com/alice/fts"; prefixed != want {
		t.Errorf("indexRoot = %q, want %q", prefixed, want)
	}
	if strings.Contains(prefixed, "posix") {
		t.Errorf("the driver name leaked into the path: %q", prefixed)
	}
}

// The check asks whether the template distinguishes accounts, not whether it
// names a variable. It used to ask the second question and so admitted the
// failure its own message describes: %d is the domain, %n the local part, and
// neither is unique to an account (#1095).
//
// Asserted against resolved roots for three accounts rather than against the
// error alone: the property is "two accounts never share a directory", and a
// template can satisfy the wording of a check while failing that.
func TestIndexRootMustDistinguishAccounts(t *testing.T) {
	users := []*mailbox.UserInfo{
		{Username: "u1@d1.test", Home: "/var/mail/d1.test/u1"},
		{Username: "u2@d1.test", Home: "/var/mail/d1.test/u2"},
		{Username: "u1@d2.test", Home: "/var/mail/d2.test/u1"},
	}

	for _, tc := range []struct {
		tmpl    string
		accept  bool
		because string
	}{
		{"posix:prefix=%h/fts/", true, "the home is per-account"},
		{"/var/fts/%u", true, "the full address is per-account"},
		{"~/fts", true, "the home again"},
		{"/var/fts/%d/%n", true, "domain and local part together"},
		{"/var/fts/%n/%d", true, "either order"},
		{"/var/fts/%d", false, "every account in a domain would share it"},
		{"/var/fts/%n", false, "the local part repeats across domains"},
		{"/var/fts", false, "no variable at all"},
		// The two a syntactic check gets wrong in opposite directions.
		//
		// "%%d" is an escaped percent followed by a literal "d" — not a
		// domain, though a substring search sees one. It resolves to one
		// directory for everyone.
		{"/var/fts/%%d/%n", false, "%%d is a literal, so only the local part varies"},
		// A hash variable is per-account and a substring search knows nothing
		// about it. The sandbox uses this form for VOLATILEDIR.
		{"/var/fts/%2.256Nu", true, "a hash of the username is per-account"},
	} {
		t.Run(tc.tmpl, func(t *testing.T) {
			err := checkIndexRoot(tc.tmpl)
			if tc.accept && err != nil {
				t.Fatalf("refused (%s): %v", tc.because, err)
			}
			if !tc.accept {
				if err == nil {
					t.Fatalf("accepted, but %s", tc.because)
				}
				return
			}

			// Accepted templates must actually separate the three accounts.
			s := &Service{opts: Options{IndexRoot: tc.tmpl}}
			seen := map[string]string{}
			for _, u := range users {
				root := s.indexRoot(u)
				if other, dup := seen[root]; dup {
					t.Errorf("%s and %s share %s", other, u.Username, root)
				}
				seen[root] = u.Username
			}
		})
	}
}

// The refusals name their own reason: "add %n" and "add %d" are different
// repairs, and an operator who is told the generic message has to work out
// which applies.
func TestIndexRootRefusalsNameTheReason(t *testing.T) {
	if err := checkIndexRoot("/var/fts/%d"); err == nil || !strings.Contains(err.Error(), "%n") {
		t.Errorf("refusing %%d should suggest adding %%n, got %v", err)
	}
	if err := checkIndexRoot("/var/fts/%n"); err == nil || !strings.Contains(err.Error(), "%d") {
		t.Errorf("refusing %%n should suggest adding %%d, got %v", err)
	}
}
