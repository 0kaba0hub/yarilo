package imap

import "testing"

// TestWardenSessionID guards #808: the warden session id used for SELECT pushes
// comes from the preamble sid captured into s.sid, not a (never-implemented)
// XCLIENT SessionID() conn method.
func TestWardenSessionID(t *testing.T) {
	cases := []struct {
		name string
		sid  string
		want string
	}{
		{"preamble sid", "AB12cd34", "AB12cd34"},
		{"no preamble (direct connect)", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{sid: tc.sid}
			if got := s.wardenSessionID(); got != tc.want {
				t.Fatalf("wardenSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPushWardenSelect_NoSidNoOp: an empty sid must not push (and must not panic
// with a nil warden client, the WardenAddr-unset case).
func TestPushWardenSelect_NoSidNoOp(t *testing.T) {
	s := &session{srv: &Server{}} // wardenClient nil, sid ""
	s.pushWardenSelect("INBOX")   // must be a silent no-op, no panic
}
