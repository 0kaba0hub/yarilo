package passwdfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/bcrypt"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// writeFile writes body to a temp passwd-file and returns its path.
func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func authenticate(db *DB, user, pass string) protocol.Result {
	req := &protocol.Request{Username: user, Password: pass, Fields: protocol.NewFields()}
	res, _ := db.Authenticate(req)
	return res
}

func TestParse(t *testing.T) {
	body := `# comment line
:no-username-here
alice@example.com:{PLAIN}secret:1000:1000:Alice:/mail/alice:/bin/sh:allow_nets=10.0.0.0/8 userdb_mail=maildir:~/Maildir
bob@example.com:{PLAIN}pw

trailing@example.com:{PLAIN}x`
	users := parse(body)
	if len(users) != 3 {
		t.Fatalf("want 3 users, got %d: %v", len(users), users)
	}
	a := users["alice@example.com"]
	if a == nil || a.password != "{PLAIN}secret" || a.home != "/mail/alice" {
		t.Fatalf("alice parsed wrong: %+v", a)
	}
	if a.extra["allow_nets"] != "10.0.0.0/8" {
		t.Errorf("allow_nets = %q", a.extra["allow_nets"])
	}
	if a.extra["userdb_mail"] != "maildir:~/Maildir" {
		t.Errorf("userdb_mail = %q", a.extra["userdb_mail"])
	}
	if users["bob@example.com"].home != "" {
		t.Errorf("bob home should be empty")
	}
}

func TestAuthenticate_Schemes(t *testing.T) {
	bhash, err := bcrypt.GenerateFromPassword([]byte("topsecret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	body := "plain@x:{PLAIN}secret\n" +
		"bcryptp@x:{BCRYPT}" + string(bhash) + "\n" +
		"bare@x:" + string(bhash) + "\n" // no prefix → autodetect bcrypt
	db, err := New(Config{Path: writeFile(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, user, pass string
		want             protocol.Result
	}{
		{"plain ok", "plain@x", "secret", protocol.ResultOK},
		{"plain wrong", "plain@x", "nope", protocol.ResultFail},
		{"bcrypt prefix ok", "bcryptp@x", "topsecret", protocol.ResultOK},
		{"bcrypt bare autodetect ok", "bare@x", "topsecret", protocol.ResultOK},
		{"bcrypt wrong", "bcryptp@x", "guess", protocol.ResultFail},
		{"unknown user", "ghost@x", "whatever", protocol.ResultNext},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authenticate(db, tc.user, tc.pass); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuthenticate_ForwardsPassdbFieldsOnly(t *testing.T) {
	// cols: user:pass:uid:gid:gecos:home:shell:extra → home is col5.
	body := "alice@x:{PLAIN}secret::::/mail/a::allow_nets=10.0.0.0/8 userdb_mail=maildir:~/M\n"
	db, err := New(Config{Path: writeFile(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	req := &protocol.Request{Username: "alice@x", Password: "secret", Fields: protocol.NewFields()}
	if res, _ := db.Authenticate(req); res != protocol.ResultOK {
		t.Fatalf("auth failed")
	}
	if v, ok := req.Fields.Get("allow_nets"); !ok || v != "10.0.0.0/8" {
		t.Errorf("allow_nets not forwarded: %q ok=%v", v, ok)
	}
	// userdb_ fields must NOT leak onto the passdb path.
	if _, ok := req.Fields.Get("userdb_mail"); ok {
		t.Errorf("userdb_mail leaked onto passdb path")
	}
	if v, ok := req.Fields.Get("home"); !ok || v != "/mail/a" {
		t.Errorf("home not set: %q ok=%v", v, ok)
	}
}

func TestLookup_Userdb(t *testing.T) {
	body := "alice@x:{PLAIN}secret:1000:1000:Alice:/mail/a:/bin/sh:userdb_mail=maildir:~/M allow_nets=10.0.0.0/8\n"
	db, err := New(Config{Path: writeFile(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	info, err := db.Lookup("alice@x")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.Username != "alice@x" {
		t.Fatalf("lookup returned %+v", info)
	}
	if info.Home != "/mail/a" {
		t.Errorf("home = %q", info.Home)
	}
	if info.MailLocation != "maildir:~/M" {
		t.Errorf("mail = %q", info.MailLocation)
	}
	// Unknown user → (nil, nil) so the chain falls through.
	miss, err := db.Lookup("ghost@x")
	if err != nil || miss != nil {
		t.Errorf("miss = %+v err=%v", miss, err)
	}
}

func TestIterate(t *testing.T) {
	body := "a@x:{PLAIN}1\nb@x:{PLAIN}2\nc@x:{PLAIN}3\n"
	db, err := New(Config{Path: writeFile(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Iterate()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 users, got %d: %v", len(got), got)
	}
}

func TestReload_OnChange(t *testing.T) {
	p := writeFile(t, "alice@x:{PLAIN}secret\n")
	db, err := New(Config{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if authenticate(db, "bob@x", "pw") != protocol.ResultNext {
		t.Fatalf("bob should not exist yet")
	}
	// Rewrite with a changed size so the mtime/size guard triggers a reload.
	if err := os.WriteFile(p, []byte("alice@x:{PLAIN}secret\nbob@x:{PLAIN}pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if authenticate(db, "bob@x", "pw") != protocol.ResultOK {
		t.Errorf("bob should exist after reload")
	}
}

func TestLookupSCRAM(t *testing.T) {
	creds, err := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	if err != nil {
		t.Fatal(err)
	}
	stored := "{SCRAM-SHA-256}" + sasl.EncodeScramCredentials(creds)
	db, err := New(Config{Path: writeFile(t, "alice@x:"+stored+"\n")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.LookupSCRAMSha256("alice@x")
	if err != nil || got == nil {
		t.Fatalf("scram lookup: got=%v err=%v", got, err)
	}
	// PLAIN path against the same SCRAM verifier also works.
	if authenticate(db, "alice@x", "hunter2") != protocol.ResultOK {
		t.Errorf("plain-path verify against SCRAM column failed")
	}
	// Non-SCRAM user → (nil, nil).
	db2, _ := New(Config{Path: writeFile(t, "bob@x:{PLAIN}pw\n")})
	if g, _ := db2.LookupSCRAMSha256("bob@x"); g != nil {
		t.Errorf("PLAIN column surfaced a SCRAM verifier")
	}
}
