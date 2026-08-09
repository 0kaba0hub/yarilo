package jmap

import (
	"encoding/json"
	"testing"
)

// envelopeProps are the properties decision A serves from the cached ENVELOPE.
const envelopeProps = `["id","subject","sentAt","messageId","inReplyTo","from","sender","to","cc","bcc","replyTo"]`

// Deleting the message files is the oracle: after that, anything still
// answered came from the cache and nothing else could have produced it.
func TestEmailGetAnswersEnvelopePropertiesFromCache(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, richMessage, 0)

	// Warm: this request parses the message and writes the envelope back.
	warm := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":`+envelopeProps+`}`))

	removeMailFiles(t, home)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":`+envelopeProps+`}`)
	if notFound, _ := got["notFound"].([]any); len(notFound) != 0 {
		t.Fatalf("envelope properties were not served from the cache: %v", got)
	}
	cached := firstEmail(t, got)

	// Same answer as the parse, field by field: a cache that returns something
	// else is worse than one that misses.
	for _, p := range []string{"subject", "sentAt", "messageId", "inReplyTo", "from", "to"} {
		w, _ := json.Marshal(warm[p])
		c, _ := json.Marshal(cached[p])
		if string(w) != string(c) {
			t.Errorf("%s from cache = %s, from parse = %s", p, c, w)
		}
	}
	if cached["subject"] != "Привіт" {
		t.Errorf("subject = %v, want the decoded word", cached["subject"])
	}
}

// The envelope does not carry references or arbitrary header fields, so a
// request naming one of them parses the message even when the cache is warm.
func TestEmailGetParsesForPropertiesTheEnvelopeDoesNotCarry(t *testing.T) {
	cases := []struct {
		name  string
		props string
	}{
		{"references", `["id","subject","references"]`},
		{"headers", `["id","subject","headers"]`},
		{"header:* form", `["id","subject","header:X-Custom:asText"]`},
		{"a structural property", `["id","subject","preview"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id, home := storedServerWithMessageAt(t, richMessage, 0)
			// Warm the cache through a request the envelope does satisfy.
			emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+envelopeProps+`}`)
			removeMailFiles(t, home)

			got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+tc.props+`}`)
			if notFound, _ := got["notFound"].([]any); len(notFound) != 1 {
				t.Errorf("%s was answered from an envelope that does not carry it: %v", tc.name, got)
			}
		})
	}
}

// encodedNameMessage puts an encoded word in a display name, which is where
// the cached and the parsed path could disagree: the envelope stores the name
// as the header spelled it.
const encodedNameMessage = "From: =?utf-8?B?0J7Qu9C10LrRgdCw0L3QtNGA?= <alex@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: hello\r\n" +
	"Message-Id: <m9@example.com>\r\n" +
	"Date: Mon, 03 Aug 2026 10:00:00 +0300\r\n" +
	"\r\n" +
	"body\r\n"

func TestEmailGetDecodesAddressNamesFromTheCache(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, encodedNameMessage, 0)

	warm := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":`+envelopeProps+`}`))
	removeMailFiles(t, home)
	cached := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":`+envelopeProps+`}`))

	name := func(email map[string]any) any {
		from, _ := email["from"].([]any)
		if len(from) != 1 {
			t.Fatalf("from = %v, want one address", email["from"])
		}
		return from[0].(map[string]any)["name"]
	}
	if name(cached) != "Олександр" {
		t.Errorf("name from cache = %v, want the decoded word", name(cached))
	}
	if name(cached) != name(warm) {
		t.Errorf("name from cache = %v, from parse = %v", name(cached), name(warm))
	}
	// A non-UTC offset: sentAt is normalised, so a passthrough would show +03:00.
	if cached["sentAt"] != "2026-08-03T07:00:00Z" {
		t.Errorf("sentAt from cache = %v, want the UTC instant", cached["sentAt"])
	}
	if cached["sentAt"] != warm["sentAt"] {
		t.Errorf("sentAt from cache = %v, from parse = %v", cached["sentAt"], warm["sentAt"])
	}
}
