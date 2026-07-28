package ring

import "testing"

func TestParseHashFormat_Valid(t *testing.T) {
	cases := []string{"%u", "%Lu", "%n", "%Ln", "%d", "%Ld", "%Ln@%Ld", "prefix-%Lu", "%Ln%%%Ld"}
	for _, c := range cases {
		if _, err := ParseHashFormat(c); err != nil {
			t.Errorf("ParseHashFormat(%q) unexpected error: %v", c, err)
		}
	}
}

func TestParseHashFormat_Invalid(t *testing.T) {
	cases := []string{"", "%", "%L", "%D", "%{user}", "%s", "%Lx", "%L%"}
	for _, c := range cases {
		if _, err := ParseHashFormat(c); err == nil {
			t.Errorf("ParseHashFormat(%q) must reject the template, got nil error", c)
		}
	}
}

func TestHashFormat_Key(t *testing.T) {
	cases := []struct {
		format, username, want string
	}{
		{"%u", "User@Example.com", "User@Example.com"},
		{"%Lu", "User@Example.com", "user@example.com"},
		{"%n", "User@Example.com", "User"},
		{"%Ln", "User@Example.com", "user"},
		{"%d", "User@Example.com", "Example.com"},
		{"%Ld", "User@Example.com", "example.com"},
		{"%Ln@%Ld", "User@Example.com", "user@example.com"},
		{"%%%Lu", "A@B", "%a@b"},
		// Domain-less user (reference semantics): %n is the whole username, %d is empty
		// so every domain-less user shares one key. Documented + tested so the first
		// local account in a deployment does not look like a bug.
		{"%n", "localuser", "localuser"},
		{"%Ln", "LocalUser", "localuser"},
		{"%d", "localuser", ""},
		{"%Ld", "LocalUser", ""},
		// First '@' wins (matches reference t_strcut / i_strchr_to_next).
		{"%n", "a@b@c", "a"},
		{"%d", "a@b@c", "b@c"},
	}
	for _, c := range cases {
		hf, err := ParseHashFormat(c.format)
		if err != nil {
			t.Fatalf("ParseHashFormat(%q): %v", c.format, err)
		}
		if got := hf.Key(c.username); got != c.want {
			t.Errorf("HashFormat(%q).Key(%q) = %q, want %q", c.format, c.username, got, c.want)
		}
	}
}

// TestRing_UserHashMatchesCanonical locks the structural invariant: the ring's own
// userHash goes through the exact same Hash(hf.Key(...)) that director.HashUsername uses,
// so ring and userDir can never diverge for the same user and format (#850).
func TestRing_UserHashMatchesCanonical(t *testing.T) {
	for _, format := range []string{"%Lu", "%u", "%Ld", "%Ln@%Ld"} {
		hf := MustParseHashFormat(format)
		r := New(hf)
		for _, u := range []string{"Alice@D.test", "bob@x.test", "localuser"} {
			if got, want := r.userHash(u), Hash(hf.Key(u)); got != want {
				t.Errorf("format %q user %q: ring.userHash=%d, canonical Hash(hf.Key)=%d", format, u, got, want)
			}
		}
	}
}

// TestHashFormat_DomainlessCollide proves the documented edge: a %d template routes
// every domain-less user to the SAME hash (empty key), deterministically.
func TestHashFormat_DomainlessCollide(t *testing.T) {
	hf := MustParseHashFormat("%Ld")
	if Hash(hf.Key("alice")) != Hash(hf.Key("bob")) {
		t.Error("domain-only template must route all domain-less users to one hash (empty key)")
	}
}
