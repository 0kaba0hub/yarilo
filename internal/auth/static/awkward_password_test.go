package static

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// A base64 shared credential is the ordinary way to generate one, and base64
// carries '+', '/' and '='. A sandbox login with such a password failed while a
// hex one worked, which puts the question here first: does the driver that
// holds the credential mishandle those bytes?
//
// The inputs are chosen to distinguish rather than to reassure: each row is a
// character that means something to some layer between the client and here --
// base64 padding, a scheme brace, a crypt(3) marker, a shell expansion, the
// TAB the auth protocol delimits with.
func TestStaticVerifiesAwkwardPasswords(t *testing.T) {
	tests := []struct {
		name string
		pass string
	}{
		{"base64 alphabet", "aB3+xY9/zQ1w"},
		{"base64 with padding", "aB3+xY9/zQ1w=="},
		{"openssl rand -base64 24 shape", "Fq7+Jd2Kx9Lm/Pw3Nv5Ry8Tz1Ab4Cd6E"},
		{"shell expansion shape", "pw${HOME}$(id)"},
		{"tab, which the auth protocol delimits with", "pw\twith\ttabs"},
		{"spaces and quotes", `pw with "quotes" and \backslash`},
		{"non-ascii", "пароль-Ω"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, err := New(Config{Password: tc.pass, DefaultScheme: "PLAIN"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := &protocol.Request{Username: "u@example", Password: tc.pass, Fields: protocol.NewFields()}
			res, err := db.Authenticate(req)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if res != protocol.ResultOK {
				t.Errorf("the credential this driver holds does not authenticate itself: %v", res)
			}

			// And the mirror: one byte off must still fail, or the row above
			// would pass on a driver that accepts anything.
			wrong := &protocol.Request{Username: "u@example", Password: tc.pass + "x", Fields: protocol.NewFields()}
			if res, _ := db.Authenticate(wrong); res == protocol.ResultOK {
				t.Error("a different password authenticated")
			}
		})
	}
}

// The two shapes a shared credential must NOT be given, and why they are not a
// bug: a stored password is read for a scheme marker before it is compared, so
// one that starts with {...} or a crypt(3) prefix is interpreted rather than
// matched literally. That is how every passdb reads a stored credential -- but
// it means an operator who generates a password that happens to start that way
// gets a credential nobody can use, silently.
func TestStaticReadsAMarkedCredentialAsItsScheme(t *testing.T) {
	tests := []struct {
		name   string
		stored string
	}{
		{"brace marker", "{notascheme}pw"},
		{"crypt(3) marker", "$2a$notreallybcrypt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, err := New(Config{Password: tc.stored, DefaultScheme: "PLAIN"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := &protocol.Request{Username: "u@example", Password: tc.stored, Fields: protocol.NewFields()}
			if res, _ := db.Authenticate(req); res == protocol.ResultOK {
				t.Error("the marker was compared literally; scheme detection is not running on the stored value")
			}
		})
	}
}
