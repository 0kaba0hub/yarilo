package jmap

import (
	"encoding/json"
	"strings"
	"testing"
)

// headerRichMessage carries the fields a client actually reaches for through
// header:*: an unsubscribe list, a spam verdict, an authentication result, a
// folded header, and a field that appears more than once.
const headerRichMessage = "Subject: =?utf-8?q?caf=C3=A9?=\r\n" +
	"From: Sender <sender@example.com>\r\n" +
	"To: Team:alice@example.com,bob@example.com;, carol@example.com\r\n" +
	"Date: Tue, 05 Aug 2026 09:30:00 +0200\r\n" +
	"Message-Id: <first@example.com>\r\n" +
	"List-Unsubscribe: <https://example.com/u/1>, <mailto:un@example.com>\r\n" +
	"X-Spam-Status: No, score=-2.1 required=5.0\r\n" +
	"Received: from a.example.com\r\n by b.example.com;\r\n Tue, 05 Aug 2026 09:29:00 +0200\r\n" +
	"Received: from c.example.com by d.example.com\r\n" +
	"\r\n" +
	"body\r\n"

// asJSON encodes without HTML escaping, so an expectation reads as the value
// rather than as \u003c.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

// headerProp asks for one property and returns it, along with whether the key
// was in the answer at all.
//
// The distinction matters and is easy to lose: a missing key and a null value
// both decode to nil, so a test that only reads the value passes whether the
// server answered "no such header" or ignored the question entirely.
func headerPropPresent(t *testing.T, s *Server, id, property string) (any, bool) {
	t.Helper()
	raw, err := json.Marshal([]string{"id", property})
	if err != nil {
		t.Fatal(err)
	}
	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+string(raw)+`}`)
	v, present := firstEmail(t, got)[property]
	return v, present
}

// headerProp is the value alone, for the cases where presence is not in doubt.
func headerProp(t *testing.T, s *Server, id, property string) any {
	t.Helper()
	v, _ := headerPropPresent(t, s, id, property)
	return v
}

// Every form of §4.1.3, against one message.
func TestHeaderFieldForms(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	for _, tc := range []struct {
		property string
		want     string // JSON
	}{
		// The reason this feature exists: fields no data type models.
		{"header:List-Unsubscribe", `" <https://example.com/u/1>, <mailto:un@example.com>"`},
		{"header:List-Unsubscribe:asURLs", `["https://example.com/u/1","mailto:un@example.com"]`},
		{"header:X-Spam-Status", `" No, score=-2.1 required=5.0"`},

		// Raw keeps the value as written; text unfolds and decodes it.
		{"header:Subject", `" =?utf-8?q?caf=C3=A9?="`},
		{"header:Subject:asText", `"café"`},

		// Structured forms.
		{"header:Message-Id:asMessageIds", `["first@example.com"]`},
		{"header:Date:asDate", `"2026-08-05T07:30:00Z"`},
		// Keys come back in the order a decoded object has them, which is
		// alphabetical; the server's own field order is not what is asserted here.
		{"header:From:asAddresses", `[{"email":"sender@example.com","name":"Sender"}]`},

		// A field that is not there answers null rather than being absent: the
		// client asked, and "no such header" is the answer.
		{"header:X-Nonexistent", `null`},
		{"header:X-Nonexistent:asURLs", `null`},
		{"header:X-Nonexistent:all", `[]`},
	} {
		t.Run(tc.property, func(t *testing.T) {
			value, present := headerPropPresent(t, s, id, tc.property)
			if !present {
				t.Fatalf("%s is missing from the answer; the client asked for it and "+
					"a header that is not there is answered null, not omitted", tc.property)
			}
			got := asJSON(t, value)
			if got != tc.want {
				t.Errorf("%s =\n got %s\nwant %s", tc.property, got, tc.want)
			}
		})
	}
}

// Without :all the answer is the last occurrence — the header added most
// recently describes the message as it arrived here. With :all, every one, in
// the order the message carries them.
func TestHeaderFieldRepeatedFields(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	last, _ := headerProp(t, s, id, "header:Received").(string)
	if last != " from c.example.com by d.example.com" {
		t.Errorf("header:Received = %q, want the last occurrence", last)
	}

	all, ok := headerProp(t, s, id, "header:Received:all").([]any)
	if !ok || len(all) != 2 {
		t.Fatalf("header:Received:all = %v, want both occurrences", all)
	}
	first, _ := all[0].(string)
	if first == "" || first[:6] != " from " {
		t.Errorf("first occurrence = %q", first)
	}
	if second, _ := all[1].(string); second != last {
		t.Errorf("the last of :all (%q) differs from the single answer (%q)", second, last)
	}
}

// A folded header is one value. asText joins it; asRaw keeps the folding,
// because that is what "raw" means.
func TestHeaderFieldFolding(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	all, _ := headerProp(t, s, id, "header:Received:all").([]any)
	if len(all) != 2 {
		t.Fatalf("want two Received fields, got %v", all)
	}
	raw, _ := all[0].(string)
	if !strings.Contains(raw, "\n") {
		t.Errorf("raw form lost the folding: %q", raw)
	}

	text, _ := headerProp(t, s, id, "header:Received:asText:all").([]any)
	if len(text) != 2 {
		t.Fatalf("want two, got %v", text)
	}
	if unfolded, _ := text[0].(string); strings.Contains(unfolded, "\n") {
		t.Errorf("asText did not unfold: %q", unfolded)
	}
}

// Groups are what asGroupedAddresses exists for; addresses outside one come
// back under a null name.
func TestHeaderFieldGroupedAddresses(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	got := asJSON(t, headerProp(t, s, id, "header:To:asGroupedAddresses"))
	const want = `[{"addresses":[{"email":"alice@example.com","name":null},` +
		`{"email":"bob@example.com","name":null}],"name":"Team"},` +
		`{"addresses":[{"email":"carol@example.com","name":null}],"name":null}]`
	if got != want {
		t.Errorf("header:To:asGroupedAddresses =\n got %s\nwant %s", got, want)
	}
}
