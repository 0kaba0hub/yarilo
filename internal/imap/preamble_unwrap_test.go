package imap

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/loginproto"
)

// TestUnwrapPreambleConn_ThroughWrappers guards #830: the #828 reorder put the
// line-length / greeting wrappers ABOVE the PreambleListener, so the server
// must still find the *PreambleConn (pre-auth state) by walking Unwrap()
// through them. Before the fix, maxLineLenConn had no Unwrap() and the walk
// stopped there → sessions started unauthenticated.
func TestUnwrapPreambleConn_ThroughWrappers(t *testing.T) {
	pc := &loginproto.PreambleConn{Username: "u@d.test", SessionID: "s1"}

	// The full co-located chain, outermost first: greeting → maxLineLen →
	// PreambleConn. ID used to sit on top and was removed with the wrapper
	// (#1375) -- the command is parsed now.
	stack := &greetingConn{Conn: &maxLineLenConn{Conn: pc, limit: 512}}

	got := unwrapPreambleConn(stack)
	if got == nil {
		t.Fatal("unwrapPreambleConn must find the *PreambleConn through the wrapper chain")
	}
	if got.Username != "u@d.test" {
		t.Fatalf("unwrapped wrong conn: username %q", got.Username)
	}

	// The specific #830 link: maxLineLen directly over PreambleConn.
	if unwrapPreambleConn(&maxLineLenConn{Conn: pc, limit: 512}) != pc {
		t.Fatal("maxLineLenConn must expose the PreambleConn via Unwrap()")
	}
}
