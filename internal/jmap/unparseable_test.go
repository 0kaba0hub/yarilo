package jmap

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// brokenMessage carries the doubled CR that made the live gate fail (#1001).
// Malformed headers are ordinary in real mail — spam, broken MUAs, truncated
// delivery — so this is not an exotic input.
const brokenMessage = "From: Alice <alice@example.com>\r\n" +
	"\r\rSubject: broken\r\n" +
	"\r\n" +
	"body\r\n"

// A message that cannot be parsed exists. Reporting it as notFound left a
// client unable to reconcile its own view: query listed it, download served it,
// IMAP showed it, and Email/get said it was not there.
func TestUnparseableMessageIsNotReportedAsMissing(t *testing.T) {
	s, id := storedServerWithMessage(t, brokenMessage, 0)

	// The property set must not decide whether the message exists.
	tests := []struct {
		name, properties string
	}{
		{"index-only properties", `,"properties":["id","size"]`},
		{"header-derived property", `,"properties":["id","subject"]`},
		{"body values", `,"fetchTextBodyValues":true`},
		{"default property set", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]`+tt.properties+`}`)
			if notFound, _ := got["notFound"].([]any); len(notFound) != 0 {
				t.Fatalf("notFound = %v — the message exists, it just cannot be parsed", notFound)
			}
			email := firstEmail(t, got)
			if email["id"] != id {
				t.Errorf("id = %v, want %s", email["id"], id)
			}
			// The index-derived half is trustworthy and must be present.
			if email["size"] == nil || email["size"] == float64(0) {
				t.Errorf("size = %v, want the indexed size", email["size"])
			}
		})
	}
}

// The acceptance criterion: the three methods agree the message exists.
func TestUnparseableMessageIsConsistentAcrossMethods(t *testing.T) {
	s, id := storedServerWithMessage(t, brokenMessage, 0)

	ids := idsOf(t, emailQuery(t, s, `{"accountId":"u1@example.com"}`))
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("Email/query returned %v, want [%s]", ids, id)
	}

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subject"]}`)
	if notFound, _ := got["notFound"].([]any); len(notFound) != 0 {
		t.Fatalf("Email/get says notFound while Email/query lists it: %v", notFound)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/u1@example.com/"+id+"/m.eml"))
	if w.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200 — it serves the bytes the other two describe", w.Code)
	}
}

// A message whose file is gone is the other condition and keeps its own answer:
// the store cannot produce it, download 404s too, so the methods still agree —
// this time that it is absent.
func TestUnreadableFileStaysNotFound(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, richMessage, 0)
	removeMailFiles(t, home)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subject"]}`)
	notFound, _ := got["notFound"].([]any)
	if len(notFound) != 1 {
		t.Errorf("notFound = %v, want the id: an unfetchable message is absent, not merely unparseable", notFound)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, downloadRequest("/jmap/download/u1@example.com/"+id+"/m.eml"))
	if w.Code != http.StatusNotFound {
		t.Errorf("download status = %d, want 404 — it must agree with Email/get", w.Code)
	}
}
