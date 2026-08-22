package director

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// #1393 diagnostics: a phantom session raises one question -- whose record is
// this -- and a count per backend cannot answer it. The only way to tell used
// to be restarting a pod and seeing what disappeared, which destroys the state
// being diagnosed.
func TestDumpDistinguishesOwnedSessionsFromReplicas(t *testing.T) {
	srv, addr := startServer(t)

	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)
	fmt.Fprint(conn, "SESSION-OPEN\tmine\tu@example.com\t10.0.0.20\timap\n")
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("SESSION-OPEN: %q", got)
	}

	srv.membership.handleRingLine([]string{
		"SESSION-OPEN", "10.0.0.99@run7", "9102", "1", "theirs", "v@example.com", "10.0.0.21", "imap",
	}, nil)
	waitFor(t, 3*time.Second, func() bool { return len(sessionIDs(srv)) == 2 })

	rec := httptest.NewRecorder()
	srv.apiDump(rec, httptest.NewRequest("GET", "/api/director/dump", nil))

	var body struct {
		Sessions []struct {
			ID     string `json:"id"`
			User   string `json:"user"`
			Local  bool   `json:"local"`
			Origin string `json:"origin"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode dump: %v -- %s", err, rec.Body.String())
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("dump lists %d sessions, want 2: %s", len(body.Sessions), rec.Body.String())
	}

	byID := map[string]struct {
		local  bool
		origin string
	}{}
	for _, s := range body.Sessions {
		byID[s.ID] = struct {
			local  bool
			origin string
		}{s.Local, s.Origin}
	}

	if !byID["mine"].local {
		t.Error("a session this director owns is not marked local")
	}
	if byID["mine"].origin != "" {
		t.Errorf("an owned session carries an origin: %q", byID["mine"].origin)
	}
	if byID["theirs"].local {
		t.Error("another director's replica is marked local")
	}
	// The same spelling the purge logs use, so a dump and a log line compare
	// without translation.
	if want := "10.0.0.99@run7:9102"; byID["theirs"].origin != want {
		t.Errorf("replica origin = %q, want %q", byID["theirs"].origin, want)
	}
}
