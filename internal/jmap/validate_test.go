package jmap

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// emailGetRaw returns the method response whether it succeeded or failed:
// callAPI treats a method error as a test failure, which is right for every
// test that expects an answer and wrong for the ones that expect a refusal.
func emailGetRaw(t *testing.T, s *Server, args string) (name string, out map[string]any) {
	t.Helper()
	w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",`+args+`,"c0"]]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) == 0 {
		t.Fatalf("no method responses: %s", w.Body)
	}
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("response name: %v", err)
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &out); err != nil {
		t.Fatalf("response args: %v", err)
	}
	return name, out
}

// A property name the server does not carry is refused, not ignored
// (RFC 8620 §5.1).
//
// Ignoring is what this replaces, and it failed in the worst way available: a
// successful response with the property absent, in which a client cannot tell
// its own typo from a property yarilo has not implemented. Those are the two
// cases it most needs to tell apart, because one is fixed by editing the
// request and the other by waiting.
func TestUnknownPropertyIsRefused(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	for _, tc := range []struct {
		name       string
		properties string
		named      string
	}{
		{"a typo", `["id","subjekt"]`, `"subjekt"`},
		{"a header form typo", `["id","header:List-Unsubscribe:asURL"]`, `"header:List-Unsubscribe:asURL"`},
		{"an empty header name", `["id","header:"]`, `"header:"`},
		{"a property of another type", `["id","role"]`, `"role"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, got := emailGetRaw(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+tc.properties+`}`)
			if name != "error" || got["type"] != "invalidArguments" {
				t.Fatalf("answered %s %v, want an invalidArguments error", name, got)
			}
			desc, _ := got["description"].(string)
			if !strings.Contains(desc, tc.named) {
				t.Errorf("description %q does not name %s — the client would have to bisect its own request", desc, tc.named)
			}
			args, _ := got["arguments"].([]any)
			if len(args) != 1 || args[0] != "properties" {
				t.Errorf("arguments = %v, want [\"properties\"]", args)
			}
		})
	}
}

// Every name the server does carry is accepted, including the header forms and
// the index-only properties. Without this the test above passes on a server
// that refuses everything.
func TestKnownPropertiesAreAccepted(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	props := []string{
		"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt",
		"headers", "messageId", "inReplyTo", "references", "sender", "from", "to",
		"cc", "bcc", "replyTo", "subject", "sentAt",
		"bodyStructure", "bodyValues", "textBody", "htmlBody", "attachments",
		"hasAttachment", "preview",
		"header:List-Unsubscribe", "header:List-Unsubscribe:asURLs",
		"header:Received:all", "header:Received:asText:all", "header:Date:asDate",
	}
	raw, err := json.Marshal(props)
	if err != nil {
		t.Fatal(err)
	}
	name, got := emailGetRaw(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+string(raw)+`}`)
	if name == "error" {
		t.Fatalf("a request naming only valid properties was refused: %v", got)
	}
	if email := firstEmail(t, got); email["id"] != id {
		t.Errorf("id = %v, want %s", email["id"], id)
	}
}

// Every unknown name is reported at once. A client that sent twenty properties
// and two typos should not have to fix one and resend to find the other.
func TestAllUnknownPropertiesAreNamed(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	_, got := emailGetRaw(t, s,
		`{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subjekt","from","prevue"]}`)
	desc, _ := got["description"].(string)
	for _, want := range []string{"subjekt", "prevue"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %q does not name %q", desc, want)
		}
	}
	if strings.Contains(desc, `"from"`) || strings.Contains(desc, `"id"`) {
		t.Errorf("description %q names a property that is valid", desc)
	}
}

// Naming no properties asks for all of them and cannot be invalid.
func TestOmittedPropertiesAreNotValidated(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	name, got := emailGetRaw(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`)
	if name == "error" {
		t.Fatalf("a request naming no properties was refused: %v", got)
	}
}

// The check happens before the work: a refused request must not have read the
// message. Asserted through the id being absent from any answer, since there is
// no answer at all.
func TestRefusedRequestReturnsNoList(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	_, got := emailGetRaw(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subjekt"]}`)
	if _, present := got["list"]; present {
		t.Errorf("a refused call answered with a list: %v", got)
	}
}
