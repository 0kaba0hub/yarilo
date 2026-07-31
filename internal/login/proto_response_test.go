package login

import (
	"net"
	"strings"
	"testing"
	"time"
)

// capConn is a net.Conn whose writes are captured in memory; only Write matters
// for exercising writeProtoError / writeProtoClose.
type capConn struct {
	net.Conn
	b strings.Builder
}

func (c *capConn) Write(p []byte) (int, error)      { return c.b.Write(p) }
func (c *capConn) SetWriteDeadline(time.Time) error { return nil }
func (c *capConn) out() string                      { return c.b.String() }

// TestWriteProtoErrorTransientKeepsConnectionOpen pins the #928 contract: a
// transient failure (imapCodeUnavailable) must be answered with the per-command
// "keep the connection open" form for every protocol — never a close
// announcement (BYE / 421), which a compliant client treats as "hang up" and
// which would defeat the #896 re-login.
func TestWriteProtoErrorTransientKeepsConnectionOpen(t *testing.T) {
	tests := []struct {
		name    string
		proto   Protocol
		tag     string
		wantHas string
		wantNot string // a close token that must NOT appear
	}{
		{"imap tagged NO", ProtocolIMAP, "a1", "a1 NO [UNAVAILABLE]", "BYE"},
		{"pop3 sys/temp", ProtocolPOP3, "", "-ERR [SYS/TEMP]", "-ERR [SYS/PERM]"},
		{"submission 454 not 421", ProtocolSubmission, "", "454 4.7.0", "421"},
		{"managesieve trylater not bye", ProtocolManageSieve, "", "NO (TRYLATER)", "BYE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &capConn{}
			writeProtoError(c, tc.proto, tc.tag, imapCodeUnavailable, "temporarily unavailable")
			out := c.out()
			if !strings.Contains(out, tc.wantHas) {
				t.Fatalf("transient response = %q, want it to contain %q", out, tc.wantHas)
			}
			if strings.Contains(out, tc.wantNot) {
				t.Fatalf("transient response = %q must NOT contain the close token %q", out, tc.wantNot)
			}
		})
	}
}

// TestWriteProtoCloseAnnouncesClose pins the counterpart: a genuine close (the
// re-login cap, a permanent misconfiguration) announces itself with the
// protocol-correct close notice, so no protocol drops the socket unannounced.
func TestWriteProtoCloseAnnouncesClose(t *testing.T) {
	tests := []struct {
		name    string
		proto   Protocol
		wantHas string
	}{
		{"imap untagged BYE", ProtocolIMAP, "* BYE"},
		{"pop3 sys/temp", ProtocolPOP3, "-ERR [SYS/TEMP]"},
		{"submission 421", ProtocolSubmission, "421 4.3.0"},
		{"managesieve BYE", ProtocolManageSieve, "BYE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &capConn{}
			writeProtoClose(c, tc.proto, "closing")
			out := c.out()
			if !strings.Contains(out, tc.wantHas) {
				t.Fatalf("close notice = %q, want it to contain %q", out, tc.wantHas)
			}
		})
	}
}
