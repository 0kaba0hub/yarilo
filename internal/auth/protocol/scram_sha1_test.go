package protocol

import (
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

// scramSha1Passdb is a stub Passdb that ALSO implements
// SCRAMSha1Lookup. Mirrors scramPassdb (SHA-256) so the chain
// walker can be exercised against the SHA-1 family without
// coupling the two test surfaces.
type scramSha1Passdb struct {
	username string
	creds    *sasl.ScramCredentials
	err      error
}

func (s *scramSha1Passdb) Authenticate(_ *Request) (Result, error) {
	return ResultNext, nil
}

func (s *scramSha1Passdb) LookupSCRAMSha1(user string) (*sasl.ScramCredentials, error) {
	if s.err != nil {
		return nil, s.err
	}
	if user == s.username {
		return s.creds, nil
	}
	return nil, nil
}

func TestChainAuthenticator_LookupSCRAMSha1(t *testing.T) {
	creds, _ := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	a := NewAuthenticator([]Passdb{
		&stubPassdb{result: ResultNext},
		&scramSha1Passdb{username: "alice", creds: creds},
	})
	lookup, ok := a.(SCRAMSha1Lookup)
	if !ok {
		t.Fatalf("Authenticator does not implement SCRAMSha1Lookup")
	}
	got, err := lookup.LookupSCRAMSha1("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Iterations != creds.Iterations {
		t.Errorf("expected hit on alice, got %+v", got)
	}
	miss, err := lookup.LookupSCRAMSha1("bob")
	if err != nil || miss != nil {
		t.Errorf("expected miss for bob: %+v err=%v", miss, err)
	}
}

func TestChainAuthenticator_LookupSCRAMSha1_PropagatesError(t *testing.T) {
	a := NewAuthenticator([]Passdb{
		&scramSha1Passdb{err: errors.New("sql conn refused")},
	})
	lookup, _ := a.(SCRAMSha1Lookup)
	_, err := lookup.LookupSCRAMSha1("alice")
	if err == nil {
		t.Errorf("expected error propagated, got nil")
	}
}

// TestChainAuthenticator_MixedSCRAMFamilies — a chain may carry
// both SHA-256 and SHA-1 verifiers in distinct passdbs (one
// passdb per digest family). Each lookup must walk only the
// matching family.
func TestChainAuthenticator_MixedSCRAMFamilies(t *testing.T) {
	sha1Creds, _ := sasl.GenerateScramSha1Credentials("pw", sasl.MinScramIterations)
	sha256Creds, _ := sasl.GenerateScramSha256Credentials("pw", sasl.MinScramIterations)
	a := NewAuthenticator([]Passdb{
		&scramSha1Passdb{username: "legacy", creds: sha1Creds},
		&scramPassdb{username: "modern", creds: sha256Creds},
	})
	sha1, _ := a.(SCRAMSha1Lookup)
	got, _ := sha1.LookupSCRAMSha1("legacy")
	if got == nil {
		t.Errorf("SHA-1 chain miss on legacy user")
	}
	// Modern user has no SHA-1 verifier in the chain.
	miss, _ := sha1.LookupSCRAMSha1("modern")
	if miss != nil {
		t.Errorf("modern user surfaced via SHA-1 lookup: %+v", miss)
	}

	sha256, _ := a.(SCRAMSha256Lookup)
	got, _ = sha256.LookupSCRAMSha256("modern")
	if got == nil {
		t.Errorf("SHA-256 chain miss on modern user")
	}
}
