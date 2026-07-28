package director

import (
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// openSession registers a session for user routed to backendIP, returning the
// captureConn that owns it so a test can assert what USER-KICKED it received.
func openSession(t *testing.T, s *Server, id, user, backendIP string) *captureConn {
	t.Helper()
	conn := &captureConn{}
	c := &client{conn: conn}
	s.handleSessionOpen(c, []string{"SESSION-OPEN", id, user, backendIP, "imap"})
	return conn
}

// TestUsernameHash_KickMatchByHash is the mirror test the design requires (#850): a
// session opened under one spelling must be selected for a kick issued under another
// spelling IFF the format folds them to the same hash. It also proves the USER-KICKED
// payload carries the SESSION's original-case username (not the kicker's input), so the
// login-side original-case match (#701) still lands.
func TestUsernameHash_KickMatchByHash(t *testing.T) {
	cases := []struct {
		name       string
		format     string
		loginUser  string // case the client logged in with
		kickUser   string // case the admin/operator kicked with
		wantKicked bool
	}{
		{"lowercase folds both spellings", "%Lu", "Alice@D.test", "alice@d.test", true},
		{"case-sensitive keeps them distinct", "%u", "Alice@D.test", "alice@d.test", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewWithOptions(Options{UsernameHashFormat: c.format})
			conn := openSession(t, s, "sess-1", c.loginUser, "10.0.0.1")

			s.kickStaleSessions(HashUsername(c.kickUser, s.hf), "10.0.0.1:993")

			got := string(conn.written)
			kicked := strings.Contains(got, "USER-KICKED\t"+c.loginUser)
			if kicked != c.wantKicked {
				t.Fatalf("format %q: login=%q kick=%q → kicked=%v, want %v (wire=%q)",
					c.format, c.loginUser, c.kickUser, kicked, c.wantKicked, got)
			}
			// When kicked, the payload must be the SESSION's original case, never the
			// kicker's lowercased input — that is what the login proxy keys on (#701).
			if c.wantKicked && strings.Contains(got, "USER-KICKED\t"+c.kickUser) && c.kickUser != c.loginUser {
				t.Errorf("USER-KICKED must echo the session's original-case user %q, not the kicker's input %q", c.loginUser, c.kickUser)
			}
		})
	}
}

// TestUsernameHash_BackCompatMatchesLowercaseBool proves the derived-template
// back-compat path is byte-identical to the old bool: no explicit format + default
// lowercase must hash exactly like an explicit "%Lu".
func TestUsernameHash_BackCompatMatchesLowercaseBool(t *testing.T) {
	back := NewWithOptions(Options{})                                     // derives %Lu from the default bool
	expl := NewWithOptions(Options{UsernameHashFormat: "%Lu"})            // explicit
	off := NewWithOptions(Options{UsernameHashLowercase: boolPtr(false)}) // derives %u

	for _, u := range []string{"Alice@D.test", "bob@x.test", "MixedCase@Host"} {
		if HashUsername(u, back.hf) != HashUsername(u, expl.hf) {
			t.Errorf("user %q: default (derived %%Lu) must equal explicit %%Lu", u)
		}
		if HashUsername(u, off.hf) != ring.Hash(ring.MustParseHashFormat("%u").Key(u)) {
			t.Errorf("user %q: lowercase=false must derive %%u", u)
		}
	}
	// The explicit path disables ingress lowercasing; the back-compat path keeps it.
	if back.hashFmtExplicit {
		t.Fatalf("default config must be back-compat (hashFmtExplicit=false), got true")
	}
	if !expl.hashFmtExplicit {
		t.Fatalf("explicit username_hash must set hashFmtExplicit=true")
	}
}

// TestUsernameHash_ExplicitDisablesIngressLowercase proves normalizeUser is a no-op
// under an explicit format, so a case-sensitive %u actually stays case-sensitive at
// ingress (the whole point of the format taking over case-folding).
func TestUsernameHash_ExplicitDisablesIngressLowercase(t *testing.T) {
	s := NewWithOptions(Options{UsernameHashFormat: "%u"})
	if got := s.normalizeUser("Alice@D"); got != "Alice@D" {
		t.Errorf("explicit format must leave ingress case untouched, got %q", got)
	}
	// And two spellings land on different hashes under %u.
	if HashUsername("Alice@D", s.hf) == HashUsername("alice@d", s.hf) {
		t.Error("%u must keep two spellings on different hashes")
	}
}

func boolPtr(b bool) *bool { return &b }
