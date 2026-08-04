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

// The headers property was in the classification and had no field behind it, so
// asking for it read the message and answered nothing: the client paid for the
// read and got an object without the property and without an error (#1034).
func TestHeadersPropertyIsAnswered(t *testing.T) {
	s, id := storedServerWithMessage(t, headerRichMessage, 0)

	value, present := headerPropPresent(t, s, id, "headers")
	if !present {
		t.Fatal("headers is missing from the answer")
	}
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("headers = %v, want the message's fields", value)
	}

	// Order is the answer, not an incidental: Received lines read as a route,
	// and a sorted or deduplicated set is a different message.
	var names []string
	for _, entry := range list {
		field, _ := entry.(map[string]any)
		name, _ := field["name"].(string)
		names = append(names, name)
	}
	want := []string{
		"Subject", "From", "To", "Date", "Message-Id",
		"List-Unsubscribe", "X-Spam-Status", "Received", "Received",
	}
	if len(names) != len(want) {
		t.Fatalf("headers = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("header %d = %q, want %q — the order is the message's", i, names[i], want[i])
		}
	}

	// The value is raw, so a folded field keeps its folding.
	for _, entry := range list {
		field, _ := entry.(map[string]any)
		if field["name"] != "Received" {
			continue
		}
		if value, _ := field["value"].(string); strings.Contains(value, "b.example.com") {
			if !strings.Contains(value, "\n") {
				t.Errorf("the folded Received lost its folding: %q", value)
			}
			return
		}
	}
	t.Error("the folded Received field is not in the list")
}

// A backslash quotes whatever follows it, inside a quoted string and inside a
// comment alike (RFC 5322 §3.2.1). Without that, an escaped quote closes the
// string early and the parser then treats the punctuation after it as group
// syntax — quietly, which is the same failure flattening would have been.
//
// Tested against the parser directly: routed through a whole Email/get, the
// grouping came out the same either way, so the assertion said nothing.
func TestGroupedAddressesQuotedPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  string
		groups []any // group names, in order
	}{
		{
			name:   "escaped quote does not close the string",
			value:  `"a \" b; c" <one@example.com>, two@example.com`,
			groups: []any{nil},
		},
		{
			name:   "escaped quote does not expose a colon",
			value:  `"a \" Group: x@example.com;" <one@example.com>`,
			groups: []any{nil},
		},
		{
			name:   "a real group is still found",
			value:  `one@example.com, Team:alice@example.com;, two@example.com`,
			groups: []any{nil, "Team", nil},
		},
		{
			name:   "escaped paren does not close a comment",
			value:  `one@example.com (a \) still comment; and: not a group)`,
			groups: []any{nil},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := groupedAddresses(tc.value)
			if len(got) != len(tc.groups) {
				t.Fatalf("%d groups, want %d: %+v", len(got), len(tc.groups), got)
			}
			for i, want := range tc.groups {
				switch want := want.(type) {
				case nil:
					if got[i].Name != nil {
						t.Errorf("group %d is named %q, want ungrouped", i, *got[i].Name)
					}
				case string:
					if got[i].Name == nil || *got[i].Name != want {
						t.Errorf("group %d = %v, want %q", i, got[i].Name, want)
					}
				}
			}
		})
	}
}
