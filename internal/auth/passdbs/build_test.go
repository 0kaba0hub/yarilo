package passdbs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

func TestBuild_PasswdFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(p, []byte("alice@x:{PLAIN}secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbs, userdbs, err := Build([]config.PassdbEntry{{Driver: "passwd-file", PasswdFile: p}})
	if err != nil {
		t.Fatal(err)
	}
	// passwd-file serves both passdb and userdb roles.
	if len(dbs) != 1 || len(userdbs) != 1 {
		t.Fatalf("want 1 passdb + 1 userdb, got %d + %d", len(dbs), len(userdbs))
	}
}

func TestBuild_Static(t *testing.T) {
	dbs, userdbs, err := Build([]config.PassdbEntry{{
		Driver:         "static",
		StaticPassword: "{PLAIN}secret",
		Fields:         map[string]string{"userdb_home": "/mail/%d/%n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || len(userdbs) != 1 {
		t.Fatalf("want 1 passdb + 1 userdb, got %d + %d", len(dbs), len(userdbs))
	}
}

func TestBuild_StaticInvalid(t *testing.T) {
	// Neither static_password nor nopassword → error.
	if _, _, err := Build([]config.PassdbEntry{{Driver: "static"}}); err == nil {
		t.Errorf("static without password or nopassword should error")
	}
}

func TestBuild_UnknownDriver(t *testing.T) {
	if _, _, err := Build([]config.PassdbEntry{{Driver: "carrier-pigeon"}}); err == nil {
		t.Errorf("unknown driver should error")
	}
}

func TestBuild_MissingPasswdFile(t *testing.T) {
	if _, _, err := Build([]config.PassdbEntry{{Driver: "passwd-file", PasswdFile: "/no/such/file"}}); err == nil {
		t.Errorf("missing passwd_file should error")
	}
}
