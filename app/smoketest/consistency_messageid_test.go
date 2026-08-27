package main

import "testing"

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
