package protocol

import "testing"

// stubPassdb is a test passdb that returns a preset result.
type stubPassdb struct {
	res *AuthResponse
	err error
}

func (s *stubPassdb) Authenticate(_, _, _ string) (*AuthResponse, error) {
	return s.res, s.err
}

func TestChain_FirstWins(t *testing.T) {
	ok := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "alice"}}
	never := &stubPassdb{res: &AuthResponse{Result: AuthFail}}
	chain := Chain{ok, never}

	res, err := chain.Authenticate("alice", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK || res.Username != "alice" {
		t.Fatalf("got %+v", res)
	}
}

func TestChain_SkipNil(t *testing.T) {
	// First passdb returns nil (unknown user) → chain must try second.
	skip := &stubPassdb{res: nil}
	ok := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "bob"}}
	chain := Chain{skip, ok}

	res, err := chain.Authenticate("bob", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthOK {
		t.Fatalf("got %+v", res)
	}
}

func TestChain_AllUnknown_ReturnsFail(t *testing.T) {
	chain := Chain{
		&stubPassdb{res: nil},
		&stubPassdb{res: nil},
	}
	res, err := chain.Authenticate("nobody", "pass", "imap")
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != AuthFail {
		t.Fatalf("expected AuthFail, got %+v", res)
	}
}

func TestChain_TempFailStopsChain(t *testing.T) {
	fail := &stubPassdb{res: &AuthResponse{Result: AuthTempFail}}
	never := &stubPassdb{res: &AuthResponse{Result: AuthOK, Username: "x"}}
	chain := Chain{fail, never}

	res, _ := chain.Authenticate("x", "pass", "imap")
	if res.Result != AuthTempFail {
		t.Fatalf("expected TempFail propagation, got %+v", res)
	}
}

func TestChain_Empty(t *testing.T) {
	chain := Chain{}
	res, err := chain.Authenticate("x", "pass", "imap")
	if err != nil || res.Result != AuthFail {
		t.Fatalf("empty chain should return AuthFail without error, got res=%+v err=%v", res, err)
	}
}
