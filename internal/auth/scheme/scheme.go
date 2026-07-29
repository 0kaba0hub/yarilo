// Package scheme verifies a plaintext password against a stored password
// string carrying an optional {SCHEME} prefix, and parses SCRAM verifier
// blobs. It is the single password-scheme implementation shared by every
// passdb driver (sql, passwd-file, ...) so the recognised scheme set and its
// semantics never drift between backends.
package scheme

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/bcrypt"
)

// Verify reports whether input matches the stored password, using PLAIN as the
// fallback scheme when no {SCHEME} prefix is present and no crypt(3) marker is
// detected.
func Verify(stored, input string) bool {
	return VerifyWithDefault(stored, input, "")
}

// VerifyWithDefault is like Verify but uses defaultScheme as the fallback when
// no {SCHEME} prefix is present and no crypt(3) marker is detected. Empty
// defaultScheme falls back to PLAIN.
//
// Recognised schemes (case-insensitive prefix):
//   - {PLAIN} / {CLEARTEXT} — literal comparison (dev only)
//   - {BCRYPT} / {BLF-CRYPT} — golang.org/x/crypto/bcrypt
//   - {SHA512-CRYPT} — crypt(3) SHA-512 ($6$salt$hash)
//   - {CRYPT} — crypt(3) autodetection by hash marker ($2*→bcrypt, $6$→sha512)
//   - {SCRAM-SHA-256} / {SCRAM-SHA-1} — re-derive StoredKey from input and the
//     stored iter/salt, then constant-time compare. The same verifier blob
//     drives both this PLAIN-path verify and the SCRAM SASL exchange.
//
// Crypt(3) autodetection applies even without a prefix: $2a$/$2b$/$2y$ →
// BCRYPT, $6$ → SHA512-CRYPT.
func VerifyWithDefault(stored, input, defaultScheme string) bool {
	name, hash := SplitWithDefault(stored, defaultScheme)
	// A "CRYPT" scheme (the reference's passwd-file default) is not a concrete
	// algorithm: resolve it to the crypt(3) family by the hash marker.
	if name == "CRYPT" {
		name, hash = SplitWithDefault(hash, "")
		if name == "PLAIN" {
			return false // unmarked bare crypt(3) (DES) is unsupported
		}
	}
	// Observed after CRYPT resolution so the label carries the concrete
	// algorithm actually executed, not the alias the column happened to use.
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

// verifyScramSha256Plain re-derives the StoredKey for the supplied plain
// password using the iter+salt from the stored blob, then constant-time
// compares against the stored StoredKey. Keeps PLAIN/LOGIN auth working against
// a {SCRAM-SHA-256} column — one verifier, two auth paths.
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

// ParseSCRAMSha256Credentials extracts the SCRAM verifier from a stored
// password carrying the {SCRAM-SHA-256} scheme. Returns (nil, false) when the
// value does not carry that scheme or the blob is malformed — callers use the
// falsy outcome to mean "this user has no SCRAM-SHA-256 verifier".
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

// Split returns the scheme name (uppercased) and the remaining hash, with
// PLAIN as the default. Autodetects the crypt(3) markers $2a$/$2b$/$2y$ and $6$.
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
