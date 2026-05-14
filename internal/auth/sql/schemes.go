package sql

import (
	"crypto/subtle"
	"strings"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"golang.org/x/crypto/bcrypt"
)

// checkPassword verifies an input password against a stored password string.
// Recognised schemes (case-insensitive prefix):
//   - {PLAIN} / {CLEARTEXT} — literal comparison (dev only)
//   - {BCRYPT} / {BLF-CRYPT} — golang.org/x/crypto/bcrypt
//   - {SHA512-CRYPT} — Linux crypt(3) SHA-512 ($6$salt$hash)
//
// Without a {SCHEME} prefix, the format is auto-detected from the crypt(3)
// prefix ($2a$/$2b$/$2y$ → BCRYPT, $6$ → SHA512-CRYPT). Otherwise treated as
// PLAIN — production deployments should always store an explicit hash.
func checkPassword(stored, input string) bool {
	scheme, hash := splitScheme(stored)
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
	return "PLAIN", stored
}
