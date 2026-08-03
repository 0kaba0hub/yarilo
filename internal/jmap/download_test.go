package jmap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func downloadRequest(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = trustedPeer
	r.Header.Set(hdrUser, "u1@example.com")
	r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	r.Header.Set(hdrProxyTTL, "4")
	return r
}

// The happy path: the blob is the message, byte for byte.
func TestDownloadStreamsTheMessage(t *testing.T) {
	s, id := emailServer(t, 0)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/u1@example.com/"+id+"/mail.eml"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if got := w.Body.String(); got != richMessage {
		t.Errorf("body is %d bytes, want the %d-byte message", len(got), len(richMessage))
	}
	// A message body must never render inline in the API's own origin.
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not nosniff")
	}
}

// Ownership is a precondition, not a check on an open handle: a blob that is
// not this user's is not found, and another account's is indistinguishable from
// one that does not exist.
func TestDownloadRefusesWhatIsNotYours(t *testing.T) {
	s, id := emailServer(t, 0)
	tests := []struct {
		name, path string
		want       int
	}{
		{"own blob", "/jmap/download/u1@example.com/" + id + "/m.eml", http.StatusOK},
		{"another account", "/jmap/download/someone@else/" + id + "/m.eml", http.StatusNotFound},
		{"unknown blob", "/jmap/download/u1@example.com/deadbeef/m.eml", http.StatusNotFound},
		{"no blob id", "/jmap/download/u1@example.com/", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, downloadRequest(tt.path))
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// The download route sits behind the same trust gate as everything else.
func TestDownloadIsBehindTheTrustGate(t *testing.T) {
	s, id := emailServer(t, 0)
	r := downloadRequest("/jmap/download/u1@example.com/" + id + "/m.eml")
	r.RemoteAddr = "203.0.113.9:40000" // outside the trusted CIDR
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// A filename cannot inject a header.
func TestDownloadFilenameIsSanitised(t *testing.T) {
	s, id := emailServer(t, 0)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/u1@example.com/"+id+`/a"b`))
	cd := w.Header().Get("Content-Disposition")
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition = %q, want the quotes balanced", cd)
	}
}

// The path parser is tested directly: a malformed path never reaches the
// handler through the mux, which normalises some of these away, but the parser
// is what stops a crafted one if the routing ever changes.
func TestParseDownloadPath(t *testing.T) {
	tests := []struct {
		name, path            string
		wantAccount, wantBlob string
		wantName              string
		wantOK                bool
	}{
		{name: "full", path: "/jmap/download/acct/blob/name.eml",
			wantAccount: "acct", wantBlob: "blob", wantName: "name.eml", wantOK: true},
		{name: "no name", path: "/jmap/download/acct/blob",
			wantAccount: "acct", wantBlob: "blob", wantName: "download", wantOK: true},
		{name: "empty name segment", path: "/jmap/download/acct/blob/",
			wantAccount: "acct", wantBlob: "blob", wantName: "download", wantOK: true},
		{name: "name with slashes", path: "/jmap/download/acct/blob/a/b.eml",
			wantAccount: "acct", wantBlob: "blob", wantName: "a/b.eml", wantOK: true},
		{name: "empty account", path: "/jmap/download//blob/n"},
		{name: "empty blob", path: "/jmap/download/acct//n"},
		{name: "blob only", path: "/jmap/download/acct"},
		{name: "wrong prefix", path: "/jmap/upload/acct/blob/n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acct, blob, name, ok := parseDownloadPath(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if acct != tt.wantAccount || blob != tt.wantBlob || name != tt.wantName {
				t.Errorf("= (%q, %q, %q), want (%q, %q, %q)",
					acct, blob, name, tt.wantAccount, tt.wantBlob, tt.wantName)
			}
		})
	}
}
