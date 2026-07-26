package imap

import "testing"

// TestAnvilSessionID guards #808: the anvil session id used for SELECT pushes
// comes from the preamble sid captured into s.sid, not a (never-implemented)
// XCLIENT SessionID() conn method.
func TestAnvilSessionID(t *testing.T) {
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
			if got := s.anvilSessionID(); got != tc.want {
				t.Fatalf("anvilSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPushAnvilSelect_NoSidNoOp: an empty sid must not push (and must not panic
// with a nil anvil client, the AnvilAddr-unset case).
func TestPushAnvilSelect_NoSidNoOp(t *testing.T) {
	s := &session{srv: &Server{}} // anvilClient nil, sid ""
	s.pushAnvilSelect("INBOX")    // must be a silent no-op, no panic
}
