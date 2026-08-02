package oauth2

import (
	"context"
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// stubValidator returns a preset (claims, err) pair from Validate.
type stubValidator struct {
	claims *Claims
	err    error
}

func (s *stubValidator) Validate(_ context.Context, _ string) (*Claims, error) {
	return s.claims, s.err
}

func TestPassdb_RequiresValidator(t *testing.T) {
	if _, err := NewPassdb(PassdbConfig{}); err == nil {
		t.Error("nil validator accepted")
	}
}

func TestPassdb_EmptyTokenSkips(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{Validator: &stubValidator{}})
	res, err := p.Authenticate(&protocol.Request{Username: "alice", Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	if res != protocol.ResultNext {
		t.Errorf("empty token: result = %v, want ResultNext", res)
	}
}

func TestPassdb_UpstreamMapsToTempFail(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{Validator: &stubValidator{err: ErrUpstream}})
	res, err := p.Authenticate(&protocol.Request{Username: "x", Password: "tkn"})
	if res != protocol.ResultTempFail {
		t.Errorf("upstream: result = %v, want ResultTempFail", res)
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("upstream err lost: %v", err)
	}
}

func TestPassdb_ValidationFailMapsToNext(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{Validator: &stubValidator{err: ErrTokenInvalid}})
	res, _ := p.Authenticate(&protocol.Request{Username: "x", Password: "tkn"})
	if res != protocol.ResultNext {
		t.Errorf("validation fail: result = %v, want ResultNext (let chain try next)", res)
	}
}

func TestPassdb_HappyPath(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "alice@example.com",
			Active:   true,
			Extra: map[string]string{
				"sub":       "12345",
				"org_id":    "tenant-42",
				"unrelated": "noise",
			},
		}},
		ExtraFields: []string{"sub", "org_id"},
	})
	req := &protocol.Request{
		Username: "alice@example.com",
		Password: "valid-token",
		Service:  "imap",
	}
	res, err := p.Authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if res != protocol.ResultOK {
		t.Errorf("happy path: result = %v, want ResultOK", res)
	}
	if v, _ := req.Fields.Get("user"); v != "alice@example.com" {
		t.Errorf("user = %q", v)
	}
	if v, _ := req.Fields.Get("userdb_sub"); v != "12345" {
		t.Errorf("userdb_sub = %q", v)
	}
	if v, _ := req.Fields.Get("userdb_org_id"); v != "tenant-42" {
		t.Errorf("userdb_org_id = %q", v)
	}
	// Claims not in ExtraFields stay off the bag.
	if _, ok := req.Fields.Get("userdb_unrelated"); ok {
		t.Errorf("unselected claim leaked")
	}
}

func TestPassdb_UsernameMismatchMapsToNext(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "bob@example.com",
		}},
	})
	res, _ := p.Authenticate(&protocol.Request{
		Username: "alice@example.com",
		Password: "tkn",
	})
	if res != protocol.ResultNext {
		t.Errorf("user mismatch: result = %v, want ResultNext", res)
	}
}

func TestPassdb_LowercaseTemplate(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "alice@example.com",
		}},
		UsernameTemplate: "%Lu",
	})
	res, _ := p.Authenticate(&protocol.Request{
		Username: "Alice@Example.COM",
		Password: "tkn",
	})
	if res != protocol.ResultOK {
		t.Errorf("lowercase template: result = %v, want ResultOK", res)
	}
}

func TestPassdb_EmptyAuthzidUsesClaim(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "alice@example.com",
		}},
	})
	req := &protocol.Request{Username: "", Password: "tkn"}
	res, _ := p.Authenticate(req)
	if res != protocol.ResultOK {
		t.Errorf("empty authzid: result = %v, want ResultOK", res)
	}
	if v, _ := req.Fields.Get("user"); v != "alice@example.com" {
		t.Errorf("user resolved from claim: got %q", v)
	}
}

func TestPassdb_ActiveAttributeCheck(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "alice@example.com",
			Extra:    map[string]string{"enabled": "false"},
		}},
		ActiveAttribute: "enabled",
		ActiveValue:     "true",
	})
	res, _ := p.Authenticate(&protocol.Request{
		Username: "alice@example.com",
		Password: "tkn",
	})
	if res != protocol.ResultNext {
		t.Errorf("inactive: result = %v, want ResultNext", res)
	}
}

func TestPassdb_MissingUsernameClaim(t *testing.T) {
	p, _ := NewPassdb(PassdbConfig{
		Validator: &stubValidator{claims: &Claims{
			Username: "", // claim absent
		}},
	})
	res, err := p.Authenticate(&protocol.Request{
		Username: "",
		Password: "tkn",
	})
	if res != protocol.ResultNext {
		t.Errorf("missing username: result = %v, want ResultNext", res)
	}
	if !errors.Is(err, ErrUsernameMissing) {
		t.Errorf("err = %v, want ErrUsernameMissing", err)
	}
}
