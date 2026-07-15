package scheme

import (
	"testing"

	"github.com/emersion/go-sasl"
)

// TestSchemes_ScramSha256_PlainPathReDerives — PLAIN/LOGIN auth
// against a {SCRAM-SHA-256} stored column works by re-deriving
// StoredKey from the input password using the stored iter+salt.
func TestSchemes_ScramSha256_PlainPathReDerives(t *testing.T) {
	creds, err := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	stored := "{SCRAM-SHA-256}" + sasl.EncodeScramCredentials(creds)

	if !Verify(stored, "hunter2") {
		t.Errorf("correct password rejected by SCRAM re-derive verify")
	}
	if Verify(stored, "WRONG") {
		t.Errorf("wrong password accepted by SCRAM verify")
	}
}

// TestSchemes_ScramSha256_MalformedBlobRejects — corrupted blob
// in the password column does not panic; it just fails verify.
func TestSchemes_ScramSha256_MalformedBlobRejects(t *testing.T) {
	cases := []string{
		"{SCRAM-SHA-256}",
		"{SCRAM-SHA-256}not-comma-separated",
		"{SCRAM-SHA-256}4096,not-base64!,xx,yy",
		"{SCRAM-SHA-256}-1,QQ==,RR==,SS==",
	}
	for _, c := range cases {
		if Verify(c, "anything") {
			t.Errorf("malformed blob %q accepted", c)
		}
	}
}

// TestParseSCRAMSha256Credentials_Roundtrip — the helper used
// by the SQL passdb's LookupSCRAMSha256 path extracts a usable
// verifier from a stored column carrying the scheme prefix.
func TestParseSCRAMSha256Credentials_Roundtrip(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	stored := "{SCRAM-SHA-256}" + sasl.EncodeScramCredentials(creds)
	got, ok := ParseSCRAMSha256Credentials(stored)
	if !ok {
		t.Fatalf("ok=false on valid SCRAM column")
	}
	if got.Iterations != creds.Iterations {
		t.Errorf("iter mismatch")
	}
	if len(got.Salt) != len(creds.Salt) {
		t.Errorf("salt len mismatch")
	}
}

// TestParseSCRAMSha256Credentials_OtherSchemesIgnored — only the
// {SCRAM-SHA-256} prefix triggers extraction. PLAIN/BCRYPT
// columns return ok=false so a chain with mixed schemes does not
// surface bogus verifiers.
func TestParseSCRAMSha256Credentials_OtherSchemesIgnored(t *testing.T) {
	for _, s := range []string{
		"{PLAIN}hunter2",
		"{BCRYPT}$2a$10$abc",
		"$2b$12$def",
		"plain-no-scheme",
	} {
		if _, ok := ParseSCRAMSha256Credentials(s); ok {
			t.Errorf("non-SCRAM scheme %q surfaced as verifier", s)
		}
	}
}
