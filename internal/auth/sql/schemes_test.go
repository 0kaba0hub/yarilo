package sql

import (
	"testing"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"golang.org/x/crypto/bcrypt"
)

func TestSplitScheme(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantScheme string
		wantHash   string
	}{
		{"explicit PLAIN", "{PLAIN}secret", "PLAIN", "secret"},
		{"explicit CLEARTEXT", "{CLEARTEXT}secret", "CLEARTEXT", "secret"},
		{"explicit BCRYPT", "{BCRYPT}$2a$10$abc", "BCRYPT", "$2a$10$abc"},
		{"explicit BLF-CRYPT alias", "{BLF-CRYPT}$2a$10$abc", "BLF-CRYPT", "$2a$10$abc"},
		{"explicit SHA512-CRYPT", "{SHA512-CRYPT}$6$salt$hash", "SHA512-CRYPT", "$6$salt$hash"},
		{"case-insensitive scheme", "{bcrypt}$2b$10$abc", "BCRYPT", "$2b$10$abc"},
		{"autodetect bcrypt $2a", "$2a$10$abc", "BCRYPT", "$2a$10$abc"},
		{"autodetect bcrypt $2b", "$2b$10$abc", "BCRYPT", "$2b$10$abc"},
		{"autodetect bcrypt $2y", "$2y$10$abc", "BCRYPT", "$2y$10$abc"},
		{"autodetect sha512-crypt $6$", "$6$salt$hash", "SHA512-CRYPT", "$6$salt$hash"},
		{"unknown $5$ falls through to PLAIN", "$5$salt$hash", "PLAIN", "$5$salt$hash"},
		{"no prefix → PLAIN", "rawpass", "PLAIN", "rawpass"},
		{"unterminated brace → PLAIN", "{NOEND", "PLAIN", "{NOEND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme, hash := splitScheme(tc.input)
			if scheme != tc.wantScheme || hash != tc.wantHash {
				t.Errorf("splitScheme(%q) = (%q, %q), want (%q, %q)",
					tc.input, scheme, hash, tc.wantScheme, tc.wantHash)
			}
		})
	}
}

func TestCheckPassword_Plain(t *testing.T) {
	cases := []struct {
		stored, input string
		want          bool
	}{
		{"{PLAIN}secret", "secret", true},
		{"{PLAIN}secret", "wrong", false},
		{"{CLEARTEXT}p@ss", "p@ss", true},
		{"rawpass", "rawpass", true},
		{"rawpass", "other", false},
	}
	for _, tc := range cases {
		got := checkPassword(tc.stored, tc.input)
		if got != tc.want {
			t.Errorf("checkPassword(%q, %q) = %v, want %v", tc.stored, tc.input, got, tc.want)
		}
	}
}

func TestCheckPassword_Bcrypt(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("topsecret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	cases := []struct {
		name, stored, input string
		want                bool
	}{
		{"with prefix correct", "{BCRYPT}" + string(hash), "topsecret", true},
		{"with prefix wrong", "{BCRYPT}" + string(hash), "guess", false},
		{"BLF-CRYPT alias", "{BLF-CRYPT}" + string(hash), "topsecret", true},
		{"autodetect bare hash correct", string(hash), "topsecret", true},
		{"autodetect bare hash wrong", string(hash), "guess", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkPassword(tc.stored, tc.input); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckPassword_SHA512Crypt(t *testing.T) {
	c := sha512_crypt.New()
	hash, err := c.Generate([]byte("topsecret"), []byte("$6$NaClNaCl"))
	if err != nil {
		t.Fatalf("sha512_crypt: %v", err)
	}
	cases := []struct {
		name, stored, input string
		want                bool
	}{
		{"with prefix correct", "{SHA512-CRYPT}" + hash, "topsecret", true},
		{"with prefix wrong", "{SHA512-CRYPT}" + hash, "guess", false},
		{"autodetect $6$ correct", hash, "topsecret", true},
		{"autodetect $6$ wrong", hash, "guess", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkPassword(tc.stored, tc.input); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckPassword_UnknownScheme(t *testing.T) {
	if checkPassword("{ARGON2}xyz", "topsecret") {
		t.Error("unknown scheme should reject, not match")
	}
}
