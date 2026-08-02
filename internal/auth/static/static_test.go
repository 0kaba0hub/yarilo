package static

import (
	"testing"

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

func authenticate(db *DB, user, pass string) protocol.Result {
	req := &protocol.Request{Username: user, Password: pass, Fields: protocol.NewFields()}
	res, _ := db.Authenticate(req)
	return res
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Errorf("empty password + no nopassword should error")
	}
	if _, err := New(Config{Password: "{PLAIN}x", Nopassword: true}); err == nil {
		t.Errorf("password + nopassword should be mutually exclusive")
	}
	if _, err := New(Config{Password: "{PLAIN}x"}); err != nil {
		t.Errorf("valid password config errored: %v", err)
	}
	if _, err := New(Config{Nopassword: true}); err != nil {
		t.Errorf("valid nopassword config errored: %v", err)
	}
}

func TestAuthenticate_SharedPassword(t *testing.T) {
	db, err := New(Config{Password: "{PLAIN}secret"})
	if err != nil {
		t.Fatal(err)
	}
	// Static matches every username: correct password → OK for anyone.
	if authenticate(db, "alice@x", "secret") != protocol.ResultOK {
		t.Errorf("alice with correct shared password should pass")
	}
	if authenticate(db, "bob@y", "secret") != protocol.ResultOK {
		t.Errorf("bob with correct shared password should pass")
	}
	// Wrong password → definitive Fail (never Next: static is a catch-all).
	if got := authenticate(db, "alice@x", "wrong"); got != protocol.ResultFail {
		t.Errorf("wrong password: got %v, want ResultFail", got)
	}
}

func TestAuthenticate_Nopassword(t *testing.T) {
	db, err := New(Config{Nopassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if authenticate(db, "anyone@x", "literally anything") != protocol.ResultOK {
		t.Errorf("nopassword should accept any password")
	}
}

func TestAuthenticate_ForwardsPassdbFieldsOnly(t *testing.T) {
	db, err := New(Config{
		Password: "{PLAIN}secret",
		Fields: map[string]string{
			"allow_nets":  "10.0.0.0/8",
			"userdb_home": "/mail/%d/%n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &protocol.Request{Username: "alice@ex.com", Password: "secret", Fields: protocol.NewFields()}
	if res, _ := db.Authenticate(req); res != protocol.ResultOK {
		t.Fatalf("auth failed")
	}
	if v, ok := req.Fields.Get("allow_nets"); !ok || v != "10.0.0.0/8" {
		t.Errorf("allow_nets not forwarded: %q ok=%v", v, ok)
	}
	if _, ok := req.Fields.Get("userdb_home"); ok {
		t.Errorf("userdb_ field leaked onto passdb path")
	}
}

func TestLookup_TemplatedFields(t *testing.T) {
	db, err := New(Config{
		Password: "{PLAIN}secret",
		Fields: map[string]string{
			"userdb_home": "/mail/%d/%n",
			"userdb_mail": "maildir:/mail/%d/%n/Maildir",
			"allow_nets":  "10.0.0.0/8", // passdb-only; must not reach userdb
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := db.Lookup("alice@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	if info.Home != "/mail/ex.com/alice" {
		t.Errorf("home = %q, want /mail/ex.com/alice", info.Home)
	}
	if info.MailLocation != "maildir:/mail/ex.com/alice/Maildir" {
		t.Errorf("mail = %q", info.MailLocation)
	}
	// Static resolves every user.
	if _, err := db.Lookup("ghost@nowhere"); err != nil {
		t.Errorf("static lookup should never miss: %v", err)
	}
}

func TestLookupSCRAM(t *testing.T) {
	creds, err := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	stored := "{SCRAM-SHA-256}" + sasl.EncodeScramCredentials(creds)
	db, err := New(Config{Password: stored})
	if err != nil {
		t.Fatal(err)
	}
	// Shared SCRAM verifier surfaces for any user.
	if got, _ := db.LookupSCRAMSha256("anyone@x"); got == nil {
		t.Errorf("shared SCRAM verifier not returned")
	}
	// PLAIN path against the shared SCRAM verifier also works.
	if authenticate(db, "anyone@x", "hunter2") != protocol.ResultOK {
		t.Errorf("plain-path verify against SCRAM password failed")
	}
	// Non-SCRAM shared password → no verifier.
	db2, _ := New(Config{Password: "{PLAIN}secret"})
	if got, _ := db2.LookupSCRAMSha256("x"); got != nil {
		t.Errorf("PLAIN password surfaced a SCRAM verifier")
	}
}
