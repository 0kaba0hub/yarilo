package sql

import (
	"crypto/subtle"
	"strings"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"golang.org/x/crypto/bcrypt"
)

// checkPassword verifies an input password against a stored password string,
// using PLAIN as the fallback scheme when no {SCHEME} prefix is present and
// no crypt(3) marker is detected.
func checkPassword(stored, input string) bool {
	return checkPasswordWithDefault(stored, input, "")
}

// checkPasswordWithDefault is like checkPassword but uses defaultScheme as
// the fallback when no {SCHEME} prefix is present and no crypt(3) marker is
// detected. Empty defaultScheme falls back to PLAIN (current behaviour).
//
// Recognised schemes (case-insensitive prefix):
//   - {PLAIN} / {CLEARTEXT} — literal comparison (dev only)
//   - {BCRYPT} / {BLF-CRYPT} — golang.org/x/crypto/bcrypt
//   - {SHA512-CRYPT} — Linux crypt(3) SHA-512 ($6$salt$hash)
//
// Crypt(3) autodetection applies even without a prefix: $2a$/$2b$/$2y$ →
// BCRYPT, $6$ → SHA512-CRYPT.
func checkPasswordWithDefault(stored, input, defaultScheme string) bool {
	scheme, hash := splitSchemeWithDefault(stored, defaultScheme)
	switch scheme {
	case "PLAIN", "CLEARTEXT":
		return subtle.ConstantTimeCompare([]byte(hash), []byte(input)) == 1
	case "BCRYPT", "BLF-CRYPT":
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(input)) == nil
	case "SHA512-CRYPT":
		return sha512_crypt.New().Verify(hash, []byte(input)) == nil
	}
	return false
}

func splitScheme(stored string) (scheme, hash string) {
	return splitSchemeWithDefault(stored, "")
}

func splitSchemeWithDefault(stored, defaultScheme string) (scheme, hash string) {
	if strings.HasPrefix(stored, "{") {
		if end := strings.Index(stored, "}"); end > 0 {
			return strings.ToUpper(stored[1:end]), stored[end+1:]
		}
	}
	switch {
	case strings.HasPrefix(stored, "$2a$"),
		strings.HasPrefix(stored, "$2b$"),
		strings.HasPrefix(stored, "$2y$"):
		return "BCRYPT", stored
	case strings.HasPrefix(stored, "$6$"):
		return "SHA512-CRYPT", stored
	}
	if defaultScheme != "" {
		return strings.ToUpper(defaultScheme), stored
	}
	return "PLAIN", stored
}
