package submission

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/loginproto"
)

// TestUnwrapPreambleConn_ThroughWorkaround guards #830: the #828 reorder put the
// workaround wrapper ABOVE the PreambleListener, so NewSession must walk to the
// *PreambleConn instead of a direct type assertion (which now sees the
// workaroundConn) — else submission sessions start unauthenticated.
func TestUnwrapPreambleConn_ThroughWorkaround(t *testing.T) {
	pc := &loginproto.PreambleConn{Username: "u@d.test", SessionID: "s1"}
	wc := &workaroundConn{Conn: pc}
	got := loginproto.UnwrapPreambleConn(wc)
	if got == nil || got.Username != "u@d.test" {
		t.Fatalf("UnwrapPreambleConn must find the PreambleConn through workaroundConn, got %+v", got)
	}
}
