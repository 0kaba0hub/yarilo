package main

import (
	"os"
	"strings"
	"testing"
)

// The reading is where this row can lie, so it is exercised against real FETCH
// responses rather than only against a cluster. A comparison that runs only in
// the sandbox hides its own reading errors until a rollout — which is the
// mistake this area was opened for (#1043, #1206).
//
// The response shapes are the ones a server actually sends: the message id is
// the last field of the ENVELOPE, the ENVELOPE is not the last thing in the
// response, and the address lists in between are nested parens.
func TestEnvelopeMessageIDReadsTheLastField(t *testing.T) {
	const addrs = `(("Sender Name" NIL "sender" "test.invalid")) ` +
		`(("Sender Name" NIL "sender" "test.invalid")) ` +
		`(("Sender Name" NIL "sender" "test.invalid")) ` +
		`(("Recipient" NIL "rcpt" "test.invalid")) NIL NIL`

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "an id is present",
			in:   `* 1 FETCH (ENVELOPE ("Mon, 1 Jan 2029 00:00:00 +0000" "hello" ` + addrs + ` NIL "<abc123@mx.example.test>"))`,
			want: "<abc123@mx.example.test>",
		},
		{
			name: "no id at all — the state this row exists to catch",
			in:   `* 1 FETCH (ENVELOPE ("Mon, 1 Jan 2029 00:00:00 +0000" "hello" ` + addrs + ` NIL NIL))`,
			want: "NIL",
		},
		{
			name: "the envelope is followed by another item",
			in: `* 1 FETCH (ENVELOPE ("Mon, 1 Jan 2029 00:00:00 +0000" "hello" ` + addrs + ` NIL "<abc123@mx.example.test>")` +
				` RFC822.SIZE 4242)`,
			want: "<abc123@mx.example.test>",
		},
		{
			name: "a paren inside the subject does not close the envelope",
			in:   `* 1 FETCH (ENVELOPE ("Mon, 1 Jan 2029 00:00:00 +0000" "re: (fwd) hello)" ` + addrs + ` NIL "<paren@x>"))`,
			want: "<paren@x>",
		},
		{
			name: "an escaped quote inside the subject",
			in:   `* 1 FETCH (ENVELOPE ("Mon, 1 Jan 2029 00:00:00 +0000" "say \"hi\"" ` + addrs + ` NIL "<quote@x>"))`,
			want: "<quote@x>",
		},
		{
			// Copied byte for byte from a sandbox on 2.3.266: a stub that
			// resembles a response and a response are different inputs.
			name: "a response as the server actually sends it",
			in: `* 1 FETCH (ENVELOPE ("Mon, 01 Jan 2024 00:00:00 +0000" "Test message 1" ` +
				`(("Sender" NIL "sender" "example.com")) (("Sender" NIL "sender" "example.com")) ` +
				`(("Sender" NIL "sender" "example.com")) ((NIL NIL "recipient" "example.com")) ` +
				`NIL NIL NIL "<1@imaptest.example.com>"))`,
			want: "<1@imaptest.example.com>",
		},
		{
			name: "no envelope in the response",
			in:   `* 1 FETCH (RFC822.SIZE 4242)`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := envelopeMessageID(tt.in); got != tt.want {
				t.Errorf("envelopeMessageID = %q, want %q", got, tt.want)
			}
		})
	}
}

// The two surfaces spell one identifier differently, and the allowance is what
// says so. Without it the row would report every message as a disagreement;
// with it too wide, a message with no id on either side would pass.
func TestMessageIDBracketsAllowance(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{"same id, imap and jmap spellings", "<abc@x>", "abc@x", true},
		{"same id, both bracketed", "<abc@x>", "<abc@x>", true},
		{"different ids", "<abc@x>", "def@x", false},
		{"imap has none", "NIL", "abc@x", false},
		{"jmap has none", "<abc@x>", "null", false},
		{"neither has one", "", "", false},
		{"brackets alone are not an id", "<>", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var eq func(string, string) bool
			for _, a := range defaultAllowances() {
				if a.field == "messageId" {
					eq = a.equal
				}
			}
			if eq == nil {
				t.Fatal("no allowance for messageId; the row would call every message a disagreement")
			}
			if got := eq(tt.left, tt.right); got != tt.want {
				t.Errorf("allowance(%q, %q) = %v, want %v", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

// The reading of the JMAP side takes what jmapCall returns: the first method's
// ARGUMENTS. Unwrapping the envelope a second time found no methodResponses,
// so the list was always empty and the row failed with one message whatever the
// server had done — on 2.3.266, which stores no id, and on the build that does.
// A check that cannot tell two states of the world apart measures nothing.
func TestJMAPMessageIDsReadsMethodArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		want    []string
		wantErr bool
	}{
		{
			// Taken off a sandbox running the candidate, verbatim apart from
			// the state token, which is a long opaque string this function
			// never reads. Invented JSON would test the invention.
			name: "a response as the server actually sends it",
			args: `{"accountId":"u1@d00001.test","state":"<elided>",` +
				`"list":[{"id":"ef36173eeccdd7e763f08835b7698877",` +
				`"messageId":["e100d63a4e272736be00cd48042dc54e@yarilo"]}],"notFound":[]}`,
			want: []string{"e100d63a4e272736be00cd48042dc54e@yarilo"},
		},
		{
			name: "messageId is null — the message has no identity",
			args: `{"accountId":"u1","state":"s1","list":[{"id":"M1","messageId":null}],"notFound":[]}`,
			want: nil,
		},
		{
			name:    "the message is not there",
			args:    `{"accountId":"u1","state":"s1","list":[],"notFound":["M1"]}`,
			wantErr: true,
		},
		{
			name:    "handed the whole envelope instead of the arguments",
			args:    `{"methodResponses":[["Email/get",{"list":[{"id":"M1","messageId":["abc@x"]}]},"c0"]]}`,
			wantErr: true, // no list at this level: the caller unwrapped twice
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jmapMessageIDs([]byte(tt.args))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// The row's own verdict must be reachable. It was not: JMAP was read first, so
// a failure there answered before the finding this row exists to report.
func TestTheMessageIDVerdictIsTheRowsOwn(t *testing.T) {
	tests := []struct {
		name    string
		imap    string
		wantErr bool
	}{
		{"stored without an id", "NIL", true},
		{"nothing read at all", "", true},
		{"stored with one", "<abc@x>", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReading(surfIMAP).field("messageId", tt.imap)
			err := messageIDVerdict(r)
			if tt.wantErr != (err != nil) {
				t.Fatalf("verdict = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "delivered without a Message-ID") {
				t.Errorf("the verdict does not say what it found: %v", err)
			}
		})
	}
}

// The row's own verdict must be asked BEFORE the second surface is read.
//
// It was not, and the cost was precise: on a server that stores no id, the row
// reported a JMAP parse failure — the same text it reported on a server that
// does store one. Two states of the world, one message; the check measured
// nothing.
//
// A source guard, because the ordering cannot be exercised without a server:
// both orders compile, both return an error on a bad JMAP, and only the text
// differs.
func TestTheIMAPVerdictIsAskedBeforeJMAPIsRead(t *testing.T) {
	src, err := os.ReadFile("consistency_messageid.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func checkConsistencyMessageID(")
	if start < 0 {
		t.Fatal("cannot find checkConsistencyMessageID; this guard is watching a function that moved")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("cannot find the end of checkConsistencyMessageID")
	}
	fn := body[start : start+end]

	verdict := strings.Index(fn, "messageIDVerdict(")
	jmap := strings.Index(fn, "jmapReadMessageID(")
	switch {
	case verdict < 0:
		t.Fatal("the row no longer asks its own verdict, so a message stored without an id is reported as something else")
	case jmap < 0:
		t.Fatal("the row no longer reads the jmap side; this guard is watching the wrong function")
	case verdict > jmap:
		t.Error("jmap is read before the imap verdict is asked: a failure on that side would answer instead of the finding this row exists to report")
	}
}
