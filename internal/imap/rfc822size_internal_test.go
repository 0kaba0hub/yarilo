package imap

import "testing"

// TestVirtualSizeFromRaw pins the CRLF-normalised octet count used as the
// legacy fallback for RFC822.SIZE: every bare LF becomes CRLF on the wire, a
// CR-preceded LF does not, so the same message always reports one size (#892).
func TestVirtualSizeFromRaw(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want uint32
	}{
		{"empty", "", 0},
		{"pure crlf unchanged", "a\r\nb\r\n", 6},
		{"pure lf gains one per line", "a\nb\n", 6},
		{"mixed only bare lf counts", "a\r\nb\n", 6},
		{"leading lf", "\nx", 3},
		{"no line endings", "hello", 5},
		{"trailing cr then lf", "x\r\n", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualSizeFromRaw([]byte(tc.raw)); got != tc.want {
				t.Errorf("virtualSizeFromRaw(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
