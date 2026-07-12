package mailbox

import "testing"

func TestFormatObjectID(t *testing.T) {
	var g [16]byte
	for i := range g {
		g[i] = byte(i)
	}
	if got, want := FormatObjectID(g), "000102030405060708090a0b0c0d0e0f"; got != want {
		t.Errorf("FormatObjectID = %q, want %q (32 lowercase hex)", got, want)
	}
	if FormatObjectID([16]byte{}) != "00000000000000000000000000000000" {
		t.Error("zero GUID should format as 32 zeros")
	}
}
