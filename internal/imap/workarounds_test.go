package imap

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

func TestParseIMAPWorkarounds(t *testing.T) {
	cases := []struct {
		input []string
		want  imapWorkarounds
	}{
		{nil, 0},
		{[]string{"tb-extra-mailbox-sep"}, workaroundTBExtraMailboxSep},
		{[]string{"tb-lsub-flags"}, workaroundTBLSUBFlags},
		{[]string{"tb-extra-mailbox-sep", "tb-lsub-flags"}, workaroundTBExtraMailboxSep | workaroundTBLSUBFlags},
		{[]string{"TB-EXTRA-MAILBOX-SEP"}, workaroundTBExtraMailboxSep},
		{[]string{"unknown"}, 0},
	}
	for _, tc := range cases {
		got := ParseIMAPWorkarounds(tc.input)
		if got != tc.want {
			t.Errorf("ParseIMAPWorkarounds(%v) = %b, want %b", tc.input, got, tc.want)
		}
	}
}

func TestIsLeaf(t *testing.T) {
	folders := []string{"INBOX", "INBOX/Sent", "INBOX/Drafts", "Trash"}
	cases := []struct {
		name string
		want bool
	}{
		{"INBOX", false},       // has children INBOX/Sent, INBOX/Drafts
		{"INBOX/Sent", true},   // no children
		{"INBOX/Drafts", true}, // no children
		{"Trash", true},        // no children
	}
	for _, tc := range cases {
		got := isLeaf(tc.name, folders)
		if got != tc.want {
			t.Errorf("isLeaf(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMailboxAttrs_TBLSUBFlags(t *testing.T) {
	folders := []string{"INBOX", "INBOX/Sent", "Trash"}

	// INBOX has children → no NoInferiors
	attrs := mailboxAttrs("INBOX", folders, workaroundTBLSUBFlags)
	for _, a := range attrs {
		if a == imaplib.MailboxAttrNoInferiors {
			t.Error("INBOX should NOT have NoInferiors (it has children)")
		}
	}

	// INBOX/Sent is a leaf → NoInferiors
	attrs = mailboxAttrs("INBOX/Sent", folders, workaroundTBLSUBFlags)
	found := false
	for _, a := range attrs {
		if a == imaplib.MailboxAttrNoInferiors {
			found = true
		}
	}
	if !found {
		t.Error("INBOX/Sent should have NoInferiors (it is a leaf)")
	}

	// workaround disabled → no attrs
	attrs = mailboxAttrs("Trash", folders, 0)
	if len(attrs) != 0 {
		t.Errorf("expected no attrs without workaround, got %v", attrs)
	}
}
