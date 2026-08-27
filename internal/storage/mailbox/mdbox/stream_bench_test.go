package mdbox

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func benchUser(b *testing.B, size int) (*userMailbox, string) {
	b.Helper()
	home := filepath.Join(b.TempDir(), "home")
	bk := New()
	u := bk.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		b.Fatal(err)
	}
	msg := "From: a@a.com\r\nSubject: s\r\n\r\n" + strings.Repeat("x", size) + "\r\n"
	fn, _, _, err := u.Save("INBOX", strings.NewReader(msg), 0, int64(len(msg)), nil, [16]byte{})
	if err != nil {
		b.Fatal(err)
	}
	return u, fn
}

// The stall's shape: a header-only read of a large message.
func BenchmarkFetchHeadOf512KB(b *testing.B) {
	u, fn := benchUser(b, 512<<10)
	buf := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc, err := u.Fetch("INBOX", fn, false)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.ReadFull(rc, buf)
		_ = rc.Close()
	}
}

// The common case, so streaming is not paid for with a regression here.
func BenchmarkFetchWholeSmall(b *testing.B) {
	u, fn := benchUser(b, 4<<10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc, err := u.Fetch("INBOX", fn, false)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
}
