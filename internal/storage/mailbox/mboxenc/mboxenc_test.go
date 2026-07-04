package mboxenc_test

import (
	"testing"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mboxenc"
)

func TestToModUTF7(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"INBOX", "INBOX"},
		{"Sent", "Sent"},
		{"", ""},
		{"&", "&-"},
		{"Téléchargements", "T&AOk-l&AOk-chargements"},
		{"日本語", "&ZeVnLIqe-"},
		{"Réunion & notes", "R&AOk-union &- notes"},
		{"a/b", "a/b"},
		// Supplementary plane (𝄞 U+1D11E MUSICAL SYMBOL G CLEF)
		{"𝄞", "&2DTdHg-"},
	}
	for _, c := range cases {
		got := mboxenc.ToModUTF7(c.in)
		if got != c.want {
			t.Errorf("ToModUTF7(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromModUTF7(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"INBOX", "INBOX"},
		{"Sent", "Sent"},
		{"", ""},
		{"&-", "&"},
		{"T&AOk-l&AOk-chargements", "Téléchargements"},
		{"&ZeVnLIqe-", "日本語"},
		{"R&AOk-union &- notes", "Réunion & notes"},
		{"&2DTdHg-", "𝄞"},
	}
	for _, c := range cases {
		got, err := mboxenc.FromModUTF7(c.in)
		if err != nil {
			t.Errorf("FromModUTF7(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("FromModUTF7(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromModUTF7_Errors(t *testing.T) {
	cases := []string{
		"&abc",  // unterminated
		"&!!!-", // invalid base64
	}
	for _, in := range cases {
		if _, err := mboxenc.FromModUTF7(in); err == nil {
			t.Errorf("FromModUTF7(%q): expected error, got nil", in)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	inputs := []string{
		"INBOX",
		"Sent",
		"Téléchargements",
		"日本語フォルダ",
		"Réunion & notes",
		"a/b/c",
		"𝄞 music",
		"",
	}
	for _, s := range inputs {
		encoded := mboxenc.ToModUTF7(s)
		decoded, err := mboxenc.FromModUTF7(encoded)
		if err != nil {
			t.Errorf("round-trip(%q): decode error: %v", s, err)
			continue
		}
		if decoded != s {
			t.Errorf("round-trip(%q): got %q after encode→decode", s, decoded)
		}
	}
}

func TestNFC(t *testing.T) {
	// é as NFD (e + combining acute) vs NFC (é, U+00E9)
	nfd := "é"
	nfc := "é"
	if got := mboxenc.NFC(nfd); got != nfc {
		t.Errorf("NFC(%q) = %q, want %q", nfd, got, nfc)
	}
	// Already NFC — no change
	if got := mboxenc.NFC(nfc); got != nfc {
		t.Errorf("NFC(already-NFC) = %q, want %q", got, nfc)
	}
	// ASCII — unchanged
	if got := mboxenc.NFC("INBOX"); got != "INBOX" {
		t.Errorf("NFC(ASCII) = %q", got)
	}
}
