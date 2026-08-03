package jmap

import (
	"encoding/json"
	"strings"
	"testing"
)

// richMessage carries what the envelope fields, the body values and the
// attachment detection all read.
const richMessage = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>, carol@example.com\r\n" +
	"Subject: =?utf-8?B?0J/RgNC40LLRltGC?=\r\n" +
	"Message-Id: <m1@example.com>\r\n" +
	"In-Reply-To: <m0@example.com>\r\n" +
	"Date: Mon, 03 Aug 2026 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b1\r\n" +
	"\r\n" +
	"--b1\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Привіт, це тіло листа.\r\n" +
	"--b1\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Привіт</p><div class=long>tail</div>\r\n" +
	"--b1\r\n" +
	"Content-Type: application/pdf; name=report.pdf\r\n" +
	"Content-Disposition: attachment; filename=report.pdf\r\n" +
	"\r\n" +
	"%PDF-1.4 fake\r\n" +
	"--b1--\r\n"

// emailServer is storedServer with one rich message in INBOX.
func emailServer(t *testing.T, ceiling uint32) (*Server, string) {
	t.Helper()
	s, id := storedServerWithMessage(t, richMessage, ceiling)
	return s, id
}

func emailGet(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",`+args+`,"c0"]]}`)
}

func firstEmail(t *testing.T, got map[string]any) map[string]any {
	t.Helper()
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list has %d emails, want 1: %v", len(list), got)
	}
	return list[0].(map[string]any)
}

// The envelope fields come from the headers, decoded.
func TestEmailGetEnvelopeFields(t *testing.T) {
	s, id := emailServer(t, 0)
	email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`))

	if email["subject"] != "Привіт" {
		t.Errorf("subject = %v, want the decoded word", email["subject"])
	}
	from, _ := email["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["email"] != "alice@example.com" {
		t.Errorf("from = %v", email["from"])
	}
	if name := from[0].(map[string]any)["name"]; name != "Alice" {
		t.Errorf("from name = %v", name)
	}
	if to, _ := email["to"].([]any); len(to) != 2 {
		t.Errorf("to has %d addresses, want 2", len(to))
	}
	// Message ids travel without their angle brackets (§4.1.2.4).
	msgID, _ := email["messageId"].([]any)
	if len(msgID) != 1 || msgID[0] != "m1@example.com" {
		t.Errorf("messageId = %v", email["messageId"])
	}
	if email["sentAt"] == nil {
		t.Error("sentAt is missing")
	}
	if email["hasAttachment"] != true {
		t.Errorf("hasAttachment = %v, want true", email["hasAttachment"])
	}
}

// The point of the lazy rule: an Email/get that asked for no body value must
// come back with none, however many parts the message has.
func TestEmailGetOmitsBodyValuesUnlessAsked(t *testing.T) {
	s, id := emailServer(t, 0)
	email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`))
	bodyValues, _ := email["bodyValues"].(map[string]any)
	if len(bodyValues) != 0 {
		t.Errorf("bodyValues = %v, want empty when none was requested", bodyValues)
	}
	// The structural lists are still there: they are metadata, not content.
	if textBody, _ := email["textBody"].([]any); len(textBody) != 1 {
		t.Errorf("textBody has %d parts, want 1", len(textBody))
	}
}

// fetchTextBodyValues brings the text part and nothing else.
func TestEmailGetFetchesOnlyTheRequestedBodyKind(t *testing.T) {
	s, id := emailServer(t, 0)
	tests := []struct {
		name, args string
		wantParts  int
		wantSubstr string
	}{
		{"text only", `"fetchTextBodyValues":true`, 1, "Привіт, це тіло"},
		{"html only", `"fetchHTMLBodyValues":true`, 1, "<p>Привіт</p>"},
		{"all", `"fetchAllBodyValues":true`, 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := firstEmail(t, emailGet(t, s,
				`{"accountId":"u1@example.com","ids":["`+id+`"],`+tt.args+`}`))
			bodyValues, _ := email["bodyValues"].(map[string]any)
			if len(bodyValues) != tt.wantParts {
				t.Fatalf("bodyValues has %d parts, want %d: %v", len(bodyValues), tt.wantParts, bodyValues)
			}
			if tt.wantSubstr == "" {
				return
			}
			for _, v := range bodyValues {
				if strings.Contains(v.(map[string]any)["value"].(string), tt.wantSubstr) {
					return
				}
			}
			t.Errorf("no body value contains %q: %v", tt.wantSubstr, bodyValues)
		})
	}
}

// The ceiling truncates, marks isTruncated, and never splits a rune — the three
// things that make a server-side cap RFC-conformant.
func TestEmailGetCeilingTruncatesAndSaysSo(t *testing.T) {
	// 5 bytes lands inside the two-byte runes of "Привіт".
	s, id := emailServer(t, 5)
	email := firstEmail(t, emailGet(t, s,
		`{"accountId":"u1@example.com","ids":["`+id+`"],"fetchTextBodyValues":true}`))
	bodyValues, _ := email["bodyValues"].(map[string]any)
	if len(bodyValues) != 1 {
		t.Fatalf("bodyValues = %v", bodyValues)
	}
	for _, v := range bodyValues {
		bv := v.(map[string]any)
		value := bv["value"].(string)
		if bv["isTruncated"] != true {
			t.Errorf("isTruncated = %v, want true", bv["isTruncated"])
		}
		if len(value) > 5 {
			t.Errorf("value is %d bytes, over the 5-byte ceiling: %q", len(value), value)
		}
		if !utf8Valid(value) {
			t.Errorf("value is not valid UTF-8: %q", value)
		}
	}
}

// A client's smaller cap wins over the ceiling; a client naming none gets the
// ceiling rather than the whole body.
func TestEmailGetClientCapInteractsWithTheCeiling(t *testing.T) {
	// The text part is ~41 bytes, so a ceiling of 20 truncates and a client cap
	// of 4 truncates further — both directions are visible.
	s, id := emailServer(t, 20)
	tests := []struct {
		name, args    string
		wantTruncated bool
		wantMaxBytes  int
	}{
		{"client smaller than the ceiling", `,"maxBodyValueBytes":4`, true, 4},
		{"client larger than the ceiling", `,"maxBodyValueBytes":10000`, true, 20},
		{"client names none", ``, true, 20},
		{"client smaller than the body", `,"maxBodyValueBytes":10000`, true, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email := firstEmail(t, emailGet(t, s,
				`{"accountId":"u1@example.com","ids":["`+id+`"],"fetchTextBodyValues":true`+tt.args+`}`))
			for _, v := range email["bodyValues"].(map[string]any) {
				bv := v.(map[string]any)
				if bv["isTruncated"] != tt.wantTruncated {
					t.Errorf("isTruncated = %v, want %t", bv["isTruncated"], tt.wantTruncated)
				}
				if n := len(bv["value"].(string)); n > tt.wantMaxBytes {
					t.Errorf("value is %d bytes, over %d", n, tt.wantMaxBytes)
				}
			}
		})
	}
}

// A preview is always present and comes from the text part with whitespace
// collapsed, so a client can render a list without fetching bodies.
func TestEmailGetPreview(t *testing.T) {
	s, id := emailServer(t, 0)
	email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`))
	preview, _ := email["preview"].(string)
	if !strings.HasPrefix(preview, "Привіт, це тіло") {
		t.Errorf("preview = %q", preview)
	}
	if strings.Contains(preview, "\r") || strings.Contains(preview, "\n") {
		t.Errorf("preview carries raw newlines: %q", preview)
	}
}

// Unknown ids are reported, not fatal, and a null ids is refused outright: it
// would select every message the account has.
func TestEmailGetIDHandling(t *testing.T) {
	s, id := emailServer(t, 0)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`","nope"]}`)
	if list, _ := got["list"].([]any); len(list) != 1 {
		t.Errorf("list has %d emails, want 1", len(list))
	}
	notFound, _ := got["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != "nope" {
		t.Errorf("notFound = %v", notFound)
	}

	w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",{"accountId":"u1@example.com","ids":null},"c0"]]}`)
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("name: %v", err)
	}
	if name != "error" {
		t.Errorf("null ids was accepted; response = %s", name)
	}
}

// The JMAP id is the message GUID, which is the same identity IMAP reports as
// EMAILID: one message is one object whichever protocol reaches it.
func TestEmailIDIsTheMessageGUID(t *testing.T) {
	s, id := emailServer(t, 0)
	email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`))
	if email["id"] != id {
		t.Errorf("id = %v, want %s", email["id"], id)
	}
	if email["blobId"] != id {
		t.Errorf("blobId = %v, want the same identity", email["blobId"])
	}
	if len(email["mailboxIds"].(map[string]any)) != 1 {
		t.Errorf("mailboxIds = %v, want exactly the folder it is in", email["mailboxIds"])
	}
}

// \Seen becomes $seen: the keyword vocabulary is JMAP's, not IMAP's.
func TestEmailKeywordsUseTheJMAPVocabulary(t *testing.T) {
	s, id := emailServer(t, 0)
	email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`))
	keywords, _ := email["keywords"].(map[string]any)
	if keywords["$seen"] != true {
		t.Errorf("keywords = %v, want $seen", keywords)
	}
	if _, ok := keywords[`\Seen`]; ok {
		t.Errorf("keywords carry the IMAP flag name: %v", keywords)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// The lazy rule, proven by removing the message from disk: a request naming
// only index-backed properties must still answer, because it never opens the
// file. A request that needs the body must fail on the same store.
func TestEmailGetDoesNotOpenTheMessageForIndexOnlyProperties(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, richMessage, 0)
	removeMailFiles(t, home)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":["id","mailboxIds","keywords","size","receivedAt"]}`)
	email := firstEmail(t, got)
	if email["id"] != id {
		t.Errorf("id = %v, want %s", email["id"], id)
	}
	if email["size"] == nil || email["size"] == float64(0) {
		t.Errorf("size = %v, want the indexed size", email["size"])
	}

	// The same store cannot answer a body request: that is what proves the
	// first call really did skip the read rather than reading a cached copy.
	body := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"fetchTextBodyValues":true}`)
	notFound, _ := body["notFound"].([]any)
	if len(notFound) != 1 {
		t.Errorf("a body request succeeded with no message on disk: %v", body)
	}
}

// Naming an envelope property does open the message: those come from headers,
// not from the index.
func TestEmailGetOpensTheMessageForEnvelopeProperties(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, richMessage, 0)
	removeMailFiles(t, home)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subject"]}`)
	if notFound, _ := got["notFound"].([]any); len(notFound) != 1 {
		t.Errorf("subject was answered without reading the message: %v", got)
	}
}
