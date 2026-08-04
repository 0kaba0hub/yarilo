package jmap

import (
	"strings"
	"testing"
)

// messageWithAttachment has a real text part and a real attachment, so every
// structural property has a non-empty true value. A response that omits the
// walk and keeps the fields would state the opposite of each of them.
const messageWithAttachment = "Subject: quarterly report\r\n" +
	"From: sender@example.com\r\n" +
	"To: u1@example.com\r\n" +
	"Content-Type: multipart/mixed; boundary=sep\r\n" +
	"\r\n" +
	"--sep\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"the numbers are attached and they are good\r\n" +
	"--sep\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=q3.pdf\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--sep--\r\n"

// A response carries the properties the client asked for and no others
// (RFC 8620 §5.1).
//
// The assertion is on absence, and that is the point. Returning an unrequested
// property is not merely untidy: the server answers it from a field it never
// computed, so hasAttachment comes back false for a message with an attachment
// and preview comes back empty for one with a body. Present, and stated as
// fact. A test checking that the requested properties are correct passes
// happily while that is true.
func TestResponseCarriesOnlyRequestedProperties(t *testing.T) {
	s, id := storedServerWithMessage(t, messageWithAttachment, 0)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","subject"]}`)
	email := firstEmail(t, got)

	if email["subject"] != "quarterly report" {
		t.Errorf("subject = %v, want the parsed subject", email["subject"])
	}
	if email["id"] != id {
		t.Errorf("id = %v, want %s", email["id"], id)
	}

	// Everything the client did not ask for. Each of these has a true,
	// non-empty value on this message, which is what makes their zero values
	// misstatements rather than omissions.
	for _, unwanted := range []string{
		"hasAttachment", "attachments", "preview", "textBody", "htmlBody",
		"bodyStructure", "bodyValues", "from", "to", "size", "receivedAt", "keywords",
	} {
		if _, present := email[unwanted]; present {
			t.Errorf("%q is in the response and was not requested (value %v)", unwanted, email[unwanted])
		}
	}
}

// The same message, asked for properly: the structural properties are true when
// they are requested. Without this the test above would pass on a server that
// answers nothing at all.
func TestStructuralPropertiesAreTrueWhenRequested(t *testing.T) {
	s, id := storedServerWithMessage(t, messageWithAttachment, 0)

	got := emailGet(t, s,
		`{"accountId":"u1@example.com","ids":["`+id+`"],"properties":["id","hasAttachment","preview","attachments"]}`)
	email := firstEmail(t, got)

	if email["hasAttachment"] != true {
		t.Errorf("hasAttachment = %v, want true — the message carries a PDF", email["hasAttachment"])
	}
	preview, _ := email["preview"].(string)
	if !strings.Contains(preview, "numbers are attached") {
		t.Errorf("preview = %q, want the text part", preview)
	}
	attachments, _ := email["attachments"].([]any)
	if len(attachments) != 1 {
		t.Errorf("attachments = %v, want one", email["attachments"])
	}
}

// Naming no properties asks for all of them, so nothing is projected away.
func TestOmittedPropertiesReturnEverything(t *testing.T) {
	s, id := storedServerWithMessage(t, messageWithAttachment, 0)

	got := emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"]}`)
	email := firstEmail(t, got)

	for _, want := range []string{"id", "subject", "from", "size", "hasAttachment", "preview", "keywords"} {
		if _, present := email[want]; !present {
			t.Errorf("%q is missing from a response that named no properties", want)
		}
	}
}
