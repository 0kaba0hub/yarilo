package jmap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func testProtocol() config.JMAPProtocolConfig {
	return config.JMAPProtocolConfig{
		BaseURL:               "https://mail.example.com",
		MaxConcurrentRequests: 10,
		MaxObjectsInGet:       500,
		MaxObjectsInSet:       500,
		MaxSizeUpload:         41943040,
		MaxSizeRequest:        10485760,
		MaxCallsInRequest:     16,
	}
}

// RFC 8620 §2 fixes the member names, so the wire form is asserted rather than
// the Go struct: a renamed json tag breaks every client silently.
func TestSessionWireShape(t *testing.T) {
	raw, err := json.Marshal(buildSession(testProtocol(), "u1@example.com"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"capabilities", "accounts", "primaryAccounts", "username",
		"apiUrl", "downloadUrl", "uploadUrl", "eventSourceUrl", "state",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("session is missing %q", k)
		}
	}
	caps, _ := got["capabilities"].(map[string]any)
	for _, urn := range []string{CapCore, CapMail} {
		if _, ok := caps[urn]; !ok {
			t.Errorf("capabilities is missing %q", urn)
		}
	}
	if got["username"] != "u1@example.com" {
		t.Errorf("username = %v", got["username"])
	}
}

// The URLs are what a client follows next, so they must carry the configured
// public origin and the placeholders the RFC defines.
func TestSessionURLsUseBaseURL(t *testing.T) {
	sess := buildSession(testProtocol(), "u1@example.com")
	tests := []struct {
		name, got, wantContains string
	}{
		{"apiUrl", sess.APIURL, "https://mail.example.com/"},
		{"downloadUrl", sess.DownloadURL, "{accountId}"},
		{"downloadUrl blob", sess.DownloadURL, "{blobId}"},
		{"uploadUrl", sess.UploadURL, "{accountId}"},
		{"eventSourceUrl", sess.EventSourceURL, "{types}"},
	}
	for _, tt := range tests {
		if !strings.Contains(tt.got, tt.wantContains) {
			t.Errorf("%s = %q, want it to contain %q", tt.name, tt.got, tt.wantContains)
		}
	}
}

// A trailing slash on the configured origin must not produce doubled slashes.
func TestSessionBaseURLTrailingSlash(t *testing.T) {
	cfg := testProtocol()
	cfg.BaseURL = "https://mail.example.com/"
	sess := buildSession(cfg, "u1@example.com")
	if strings.Contains(sess.APIURL, "//jmap") {
		t.Errorf("apiUrl has a doubled slash: %q", sess.APIURL)
	}
}

// The advertised limits come from config, since a client batches against them.
func TestSessionCoreLimitsFromConfig(t *testing.T) {
	sess := buildSession(testProtocol(), "u1@example.com")
	core, ok := sess.Capabilities[CapCore].(CoreCapability)
	if !ok {
		t.Fatalf("core capability has type %T", sess.Capabilities[CapCore])
	}
	if core.MaxSizeUpload != 41943040 {
		t.Errorf("maxSizeUpload = %d", core.MaxSizeUpload)
	}
	if core.MaxObjectsInGet != 500 || core.MaxCallsInRequest != 16 {
		t.Errorf("limits not carried: %+v", core)
	}
	if len(core.CollationAlgorithms) == 0 {
		t.Error("collationAlgorithms must not be empty")
	}
}

// The account is the user's own and mail is its primary account.
func TestSessionAccount(t *testing.T) {
	const user = "u1@example.com"
	sess := buildSession(testProtocol(), user)
	acct, ok := sess.Accounts[user]
	if !ok {
		t.Fatalf("no account for %q: %v", user, sess.Accounts)
	}
	if !acct.IsPersonal || acct.IsReadOnly {
		t.Errorf("account flags: %+v", acct)
	}
	if _, ok := acct.AccountCapabilities[CapMail]; !ok {
		t.Error("account is missing the mail capability")
	}
	if sess.PrimaryAccounts[CapMail] != user {
		t.Errorf("primaryAccounts[mail] = %q", sess.PrimaryAccounts[CapMail])
	}
}

// Unlimited mail limits must serialise as null, not 0: a client reads 0 as
// "none allowed".
func TestMailCapabilityUnlimitedIsNull(t *testing.T) {
	raw, err := json.Marshal(mailCapability())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"maxMailboxesPerEmail", "maxMailboxDepth"} {
		if v, ok := got[k]; !ok || v != nil {
			t.Errorf("%s = %v, want null", k, v)
		}
	}
}

// The state changes when an advertised limit does, and only then: a client
// caches the session against it.
func TestSessionStateTracksConfig(t *testing.T) {
	base := sessionState(testProtocol())
	if base != sessionState(testProtocol()) {
		t.Error("state is not stable for identical config")
	}
	changed := testProtocol()
	changed.MaxObjectsInGet = 100
	if sessionState(changed) == base {
		t.Error("state did not change with an advertised limit")
	}
	rebased := testProtocol()
	rebased.BaseURL = "https://other.example.com"
	if sessionState(rebased) == base {
		t.Error("state did not change with the base URL")
	}
}
