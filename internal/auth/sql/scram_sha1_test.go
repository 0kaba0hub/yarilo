package sql

import (
	"testing"

	"github.com/emersion/go-sasl"
)

// TestSchemes_ScramSha1_PlainPathReDerives — PLAIN/LOGIN auth
// against a {SCRAM-SHA-1} stored column works by re-deriving
// StoredKey from the input password using the stored iter+salt.
func TestSchemes_ScramSha1_PlainPathReDerives(t *testing.T) {
	creds, err := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	stored := "{SCRAM-SHA-1}" + sasl.EncodeScramCredentials(creds)

	if !checkPassword(stored, "hunter2") {
		t.Errorf("correct password rejected by SHA-1 SCRAM re-derive verify")
	}
	if checkPassword(stored, "WRONG") {
		t.Errorf("wrong password accepted by SHA-1 SCRAM verify")
	}
}

func TestSchemes_ScramSha1_MalformedBlobRejects(t *testing.T) {
	cases := []string{
		"{SCRAM-SHA-1}",
		"{SCRAM-SHA-1}not-comma-separated",
		"{SCRAM-SHA-1}4096,not-base64!,xx,yy",
		"{SCRAM-SHA-1}-1,QQ==,RR==,SS==",
	}
	for _, c := range cases {
		if checkPassword(c, "anything") {
			t.Errorf("malformed blob %q accepted", c)
		}
	}
}

func TestParseSCRAMSha1Credentials_Roundtrip(t *testing.T) {
	creds, _ := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	stored := "{SCRAM-SHA-1}" + sasl.EncodeScramCredentials(creds)
	got, ok := ParseSCRAMSha1Credentials(stored)
	if !ok {
		t.Fatalf("ok=false on valid SHA-1 SCRAM column")
	}
	if got.Iterations != creds.Iterations {
		t.Errorf("iter mismatch")
	}
	if len(got.Salt) != len(creds.Salt) {
		t.Errorf("salt len mismatch")
	}
}

// TestParseSCRAMSha1_AndSha256_DoNotCrossTalk — the SHA-1 column
// must not parse as a SHA-256 verifier and vice versa, even though
// the inner blob shape is identical. Scheme prefix is the gate.
func TestParseSCRAMSha1_AndSha256_DoNotCrossTalk(t *testing.T) {
	sha1Creds, _ := sasl.GenerateScramSha1Credentials("pw", sasl.MinScramIterations)
	sha1Stored := "{SCRAM-SHA-1}" + sasl.EncodeScramCredentials(sha1Creds)
	if _, ok := ParseSCRAMSha256Credentials(sha1Stored); ok {
		t.Errorf("SHA-1 blob surfaced via ParseSCRAMSha256Credentials")
	}

	sha256Creds, _ := sasl.GenerateScramSha256Credentials("pw", sasl.MinScramIterations)
	sha256Stored := "{SCRAM-SHA-256}" + sasl.EncodeScramCredentials(sha256Creds)
	if _, ok := ParseSCRAMSha1Credentials(sha256Stored); ok {
		t.Errorf("SHA-256 blob surfaced via ParseSCRAMSha1Credentials")
	}
}
