// Package scheme verifies a plaintext password against a stored {SCHEME}-prefixed
// password string and parses SCRAM verifier blobs. Shared by every passdb driver
// so the recognised scheme set stays uniform across backends.
package scheme

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/bcrypt"
)

// Verify reports whether input matches the stored password, defaulting to PLAIN
// when no {SCHEME} prefix and no crypt(3) marker are present.
func Verify(stored, input string) bool {
	return VerifyWithDefault(stored, input, "")
}

// VerifyWithDefault is Verify with a configurable fallback scheme; empty
// defaultScheme falls back to PLAIN.
//
// Recognised schemes (case-insensitive prefix):
//   - {PLAIN} / {CLEARTEXT} — literal comparison (dev only)
//   - {BCRYPT} / {BLF-CRYPT} — golang.org/x/crypto/bcrypt
//   - {SHA512-CRYPT} — crypt(3) SHA-512 ($6$salt$hash)
//   - {CRYPT} — crypt(3) autodetection by hash marker ($2*→bcrypt, $6$→sha512)
//   - {SCRAM-SHA-256} / {SCRAM-SHA-1} — re-derive StoredKey from input and the
//     stored iter/salt, then constant-time compare; the same verifier blob
//     also drives the SCRAM SASL exchange.
//
// crypt(3) markers $2a$/$2b$/$2y$ → BCRYPT and $6$ → SHA512-CRYPT are autodetected
// without a prefix.
func VerifyWithDefault(stored, input, defaultScheme string) bool {
	name, hash := SplitWithDefault(stored, defaultScheme)
	// CRYPT is an alias, not an algorithm: resolve to the crypt(3) family by marker.
	if name == "CRYPT" {
		name, hash = SplitWithDefault(hash, "")
		if name == "PLAIN" {
			return false // unmarked bare crypt(3) (DES) is unsupported
		}
	}
	// Observe after resolution so the label carries the algorithm actually run.
	start := time.Now()
	defer func() { observeVerify(name, start) }()
	switch name {
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

// verifyScramSha256Plain re-derives the StoredKey from the plain password using
// the blob's iter+salt and constant-time compares, letting PLAIN/LOGIN auth work
// against a {SCRAM-SHA-256} column.
func verifyScramSha256Plain(blob, plaintext string) bool {
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return false
	}
	derived := sasl.DeriveScramSha256Credentials(plaintext, creds.Salt, creds.Iterations)
	return subtle.ConstantTimeCompare(derived.StoredKey, creds.StoredKey) == 1
}

// verifyScramSha1Plain mirrors verifyScramSha256Plain for the SHA-1 family.
func verifyScramSha1Plain(blob, plaintext string) bool {
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return false
	}
	derived := sasl.DeriveScramSha1Credentials(plaintext, creds.Salt, creds.Iterations)
	return subtle.ConstantTimeCompare(derived.StoredKey, creds.StoredKey) == 1
}

// ParseSCRAMSha256Credentials extracts the SCRAM verifier from a {SCRAM-SHA-256}
// stored password. Returns (nil, false) when the scheme differs or the blob is
// malformed, meaning the user has no SCRAM-SHA-256 verifier.
func ParseSCRAMSha256Credentials(stored string) (*sasl.ScramCredentials, bool) {
	return parseSCRAMCredentials(stored, "SCRAM-SHA-256")
}

// ParseSCRAMSha1Credentials is the SHA-1 counterpart.
func ParseSCRAMSha1Credentials(stored string) (*sasl.ScramCredentials, bool) {
	return parseSCRAMCredentials(stored, "SCRAM-SHA-1")
}

func parseSCRAMCredentials(stored, want string) (*sasl.ScramCredentials, bool) {
	name, blob := Split(stored)
	if name != want {
		return nil, false
	}
	creds, err := sasl.DecodeScramCredentials(blob)
	if err != nil {
		return nil, false
	}
	return creds, true
}

// Split returns the uppercased scheme name and remaining hash, defaulting to
// PLAIN. Autodetects the crypt(3) markers $2a$/$2b$/$2y$ and $6$.
func Split(stored string) (name, hash string) {
	return SplitWithDefault(stored, "")
}

// SplitWithDefault is Split with a configurable fallback scheme.
func SplitWithDefault(stored, defaultScheme string) (name, hash string) {
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
