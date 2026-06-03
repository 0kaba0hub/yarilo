package sql

import (
	"crypto/subtle"
	"strings"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/emersion/go-sasl"
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
//   - {SCRAM-SHA-256} — re-derive StoredKey from input + stored
//     iter/salt and constant-time compare. The same verifier blob
//     drives both this PLAIN-path verify AND the SCRAM-SHA-256
//     SASL challenge-response (see ParseSCRAMSha256Credentials),
//     so operators store one column for both flows.
//   - {SCRAM-SHA-1} — same dual-use shape as SCRAM-SHA-256 but
//     with the SHA-1 digest. Provided for legacy clients only
//     (older Thunderbird, Apple Mail fallback); new deployments
//     should provision SCRAM-SHA-256.
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
	case "SCRAM-SHA-256":
		return verifyScramSha256Plain(hash, input)
	case "SCRAM-SHA-1":
		return verifyScramSha1Plain(hash, input)
	}
	return false
}

// verifyScramSha256Plain re-derives the StoredKey for the
// supplied plain password using the iter+salt from the stored
// blob, then constant-time compares against the stored
// StoredKey. Used so PLAIN/LOGIN auth keeps working against a
// {SCRAM-SHA-256} column — operators store one verifier, two
// auth paths use it.
func verifyScramSha256Plain(blob, plaintext string) bool {
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return false
	}
	derived := sasl.DeriveScramSha256Credentials(plaintext, creds.Salt, creds.Iterations)
	return subtle.ConstantTimeCompare(derived.StoredKey, creds.StoredKey) == 1
}

// ParseSCRAMSha256Credentials extracts the SCRAM verifier from a
// stored password column carrying the `{SCRAM-SHA-256}` scheme.
// Returns (nil, false) when the value does not carry that scheme
// or the blob is malformed — callers use the falsy outcome to
// mean "this user does not have a SCRAM verifier".
func ParseSCRAMSha256Credentials(stored string) (*sasl.ScramCredentials, bool) {
	return parseSCRAMCredentials(stored, "SCRAM-SHA-256")
}

// ParseSCRAMSha1Credentials is the SHA-1 counterpart used by the
// passdb to satisfy LookupSCRAMSha1.
func ParseSCRAMSha1Credentials(stored string) (*sasl.ScramCredentials, bool) {
	return parseSCRAMCredentials(stored, "SCRAM-SHA-1")
}

func parseSCRAMCredentials(stored, want string) (*sasl.ScramCredentials, bool) {
	scheme, blob := splitScheme(stored)
	if scheme != want {
		return nil, false
	}
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return nil, false
	}
	return creds, true
}

// verifyScramSha1Plain mirrors verifyScramSha256Plain for the
// SHA-1 verifier family.
func verifyScramSha1Plain(blob, plaintext string) bool {
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return false
	}
	derived := sasl.DeriveScramSha1Credentials(plaintext, creds.Salt, creds.Iterations)
	return subtle.ConstantTimeCompare(derived.StoredKey, creds.StoredKey) == 1
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
