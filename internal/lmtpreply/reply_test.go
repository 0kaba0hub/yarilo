package lmtpreply

import (
	"testing"

	goSmtp "github.com/emersion/go-smtp"
)

func TestStripRcptPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rcpt string
		want string
	}{
		{"strips matching prefix", "<u@x> Mailbox full", "u@x", "Mailbox full"},
		{"no prefix unchanged", "Mailbox full", "u@x", "Mailbox full"},
		{"different rcpt unchanged", "<a@x> Mailbox full", "b@x", "<a@x> Mailbox full"},
		{"only first occurrence", "<u@x> <u@x> full", "u@x", "<u@x> full"},
		{"empty message", "", "u@x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &goSmtp.SMTPError{Code: 452, Message: tc.in}
			got := StripRcptPrefix(e, tc.rcpt)
			if got.Message != tc.want {
				t.Errorf("StripRcptPrefix(%q, %q) = %q, want %q", tc.in, tc.rcpt, got.Message, tc.want)
			}
			// Original must not be mutated when a prefix was stripped.
			if got != e && e.Message != tc.in {
				t.Errorf("original SMTPError mutated: %q", e.Message)
			}
		})
	}
}

func TestStripRcptPrefix_Nil(t *testing.T) {
	if StripRcptPrefix(nil, "u@x") != nil {
		t.Error("nil error should return nil")
	}
}
