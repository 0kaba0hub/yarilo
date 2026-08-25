package msgcache

import (
	"fmt"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func benchFolder(b *testing.B, n int) (mailbox.UserIndex, *mailbox.Folder, []*mailbox.MessageMeta) {
	b.Helper()
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: b.TempDir()})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		b.Fatal(err)
	}
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		b.Fatal("cache unavailable")
	}
	for uid := uint32(1); uid <= uint32(n); uid++ {
		m := &mailbox.MessageMeta{UID: uid}
		if err := idx.AppendMessage(f.ID, m); err != nil {
			b.Fatal(err)
		}
		fc.StoreEnvelope(m, &imaplib.Envelope{
			Subject: fmt.Sprintf("Subject %d", uid), MessageID: fmt.Sprintf("<m%d@x>", uid),
			Date: time.Unix(1770000000+int64(uid), 0).UTC(),
		})
		fc.StoreReferences(m, []string{fmt.Sprintf("<m%d@x>", uid-1)})
	}
	fc.Close()

	metas, err := idx.GetMessages(f.ID, nil)
	if err != nil {
		b.Fatal(err)
	}
	return idx, f, metas
}

// Two accessors, two full reads of the same record: each walks the chain, does
// its own ReadAt and decodes every field. Threading needs both for every
// message, so this is the difference between one pass and two over a whole
// mailbox -- ~170ms of a 320ms THREAD on ten thousand messages (#1461).
func BenchmarkTwoReadsPerMessage(b *testing.B) {
	idx, f, metas := benchFolder(b, 2000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
		for _, m := range metas {
			_ = fc.Envelope(m)
			_, _ = fc.References(m)
		}
		fc.Close()
	}
}

func BenchmarkOneReadPerMessage(b *testing.B) {
	idx, f, metas := benchFolder(b, 2000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
		for _, m := range metas {
			_, _, _ = fc.EnvelopeAndReferences(m)
		}
		fc.Close()
	}
}
