package protocol

import (
	"errors"
	"testing"
)

// stubUserdb is a single-user fake for chain composition tests.
// Returning a nil Result with no error simulates the "not in this
// backend" case the chain falls through on.
type stubUserdb struct {
	user string
	info *UserInfo
	err  error
}

func (s *stubUserdb) Lookup(username string) (*UserInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.user != "" && username != s.user {
		return nil, nil
	}
	return s.info, nil
}

func TestUserdbChain_FirstHitWins(t *testing.T) {
	first := &stubUserdb{user: "alice", info: &UserInfo{Username: "alice", Home: "/h/a"}}
	second := &stubUserdb{user: "alice", info: &UserInfo{Username: "alice", Home: "/h/b-should-not-see"}}
	chain := UserdbChain{first, second}

	got, err := chain.Lookup("alice")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil || got.Home != "/h/a" {
		t.Errorf("got %+v, want first backend's UserInfo", got)
	}
}

func TestUserdbChain_FallsThroughOnNilResult(t *testing.T) {
	missing := &stubUserdb{user: "bob"} // returns nil for alice
	hit := &stubUserdb{user: "alice", info: &UserInfo{Username: "alice", UID: 1000}}
	chain := UserdbChain{missing, hit}

	got, err := chain.Lookup("alice")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil || got.UID != 1000 {
		t.Errorf("got %+v, want second backend's UserInfo with UID=1000", got)
	}
}

func TestUserdbChain_AllMissReturnsNil(t *testing.T) {
	chain := UserdbChain{
		&stubUserdb{user: "carol"},
		&stubUserdb{user: "dave"},
	}
	got, err := chain.Lookup("alice")
	if err != nil {
		t.Errorf("Lookup on unknown user: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unknown user, got %+v", got)
	}
}

func TestUserdbChain_ErrorShortCircuits(t *testing.T) {
	sentinel := errors.New("backend broken")
	hit := &stubUserdb{user: "alice", info: &UserInfo{Username: "alice"}}
	chain := UserdbChain{
		&stubUserdb{err: sentinel}, // first errors
		hit,                        // never reached
	}
	got, err := chain.Lookup("alice")
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil UserInfo on error, got %+v", got)
	}
}

func TestUserdbChain_EmptyChain(t *testing.T) {
	chain := UserdbChain{}
	got, err := chain.Lookup("alice")
	if err != nil || got != nil {
		t.Errorf("empty chain: got %+v err=%v, want nil/nil", got, err)
	}
}
