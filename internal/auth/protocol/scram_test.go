package protocol

import (
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

// scramPassdb is a stub Passdb that ALSO implements
// SCRAMSha256Lookup. Used to verify chainAuthenticator finds
// SCRAM-capable entries in a mixed chain.
type scramPassdb struct {
	username string
	creds    *sasl.ScramCredentials
	err      error
}

func (s *scramPassdb) Authenticate(req *Request) (Result, error) {
	return ResultNext, nil
}

func (s *scramPassdb) LookupSCRAMSha256(user string) (*sasl.ScramCredentials, error) {
	if s.err != nil {
		return nil, s.err
	}
	if user == s.username {
		return s.creds, nil
	}
	return nil, nil
}

// TestChainAuthenticator_LookupSCRAMSha256 walks the chain and
// returns the verifier from the first SCRAM-capable entry that
// matches the user.
func TestChainAuthenticator_LookupSCRAMSha256(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	a := NewAuthenticator([]Passdb{
		&stubPassdb{result: ResultNext},               // non-SCRAM, skipped
		&scramPassdb{username: "alice", creds: creds}, // hit
	})
	lookup, ok := a.(SCRAMSha256Lookup)
	if !ok {
		t.Fatalf("Authenticator does not implement SCRAMSha256Lookup")
	}
	got, err := lookup.LookupSCRAMSha256("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Iterations != creds.Iterations {
		t.Errorf("expected hit on alice, got %+v", got)
	}
	miss, err := lookup.LookupSCRAMSha256("bob")
	if err != nil || miss != nil {
		t.Errorf("expected miss for bob: %+v err=%v", miss, err)
	}
}

// TestChainAuthenticator_LookupSCRAMSha256_PropagatesError —
// transient backend error from a chain entry must propagate so
// the session surfaces temp_fail.
func TestChainAuthenticator_LookupSCRAMSha256_PropagatesError(t *testing.T) {
	a := NewAuthenticator([]Passdb{
		&scramPassdb{err: errors.New("sql conn refused")},
	})
	lookup, _ := a.(SCRAMSha256Lookup)
	_, err := lookup.LookupSCRAMSha256("alice")
	if err == nil {
		t.Errorf("expected error propagated, got nil")
	}
}

// TestChainAuthenticator_LookupSCRAMSha256_NoSCRAMPassdbs — chain
// with no SCRAM-capable entries returns (nil, nil) — the session
// uses this to decide whether to advertise SCRAM mechs.
func TestChainAuthenticator_LookupSCRAMSha256_NoSCRAMPassdbs(t *testing.T) {
	a := NewAuthenticator([]Passdb{
		&stubPassdb{result: ResultNext},
		&stubPassdb{result: ResultFail},
	})
	lookup, _ := a.(SCRAMSha256Lookup)
	got, err := lookup.LookupSCRAMSha256("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for chain without SCRAM passdbs, got %+v", got)
	}
}

// TestPlainOnlyAuthenticator_ForwardsSCRAMLookup — even when
// master-users are disabled (and the wrapper hides
// MasterAuthenticator), SCRAM lookup remains accessible.
// SCRAM is orthogonal to master-user impersonation.
func TestPlainOnlyAuthenticator_ForwardsSCRAMLookup(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	// Default: master-users DISABLED → wrapper returned.
	a := NewAuthenticator([]Passdb{
		&scramPassdb{username: "alice", creds: creds},
	})
	if _, ok := a.(MasterAuthenticator); ok {
		t.Errorf("master-users disabled but MasterAuthenticator exposed")
	}
	lookup, ok := a.(SCRAMSha256Lookup)
	if !ok {
		t.Fatalf("plainOnlyAuthenticator does not forward SCRAMSha256Lookup")
	}
	got, _ := lookup.LookupSCRAMSha256("alice")
	if got == nil {
		t.Errorf("forward lookup returned nil for known user")
	}
}
