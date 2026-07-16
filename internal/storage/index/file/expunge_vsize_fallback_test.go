package file

import (
	"os"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestExpungeVSizeFallbackNoExt reproduces #570: a message whose per-record
// vsize extension is absent (e.g. delivered by another process without a
// virtual size, its physical size only in the .names sidecar). The aggregate
// self-heals to the physical size via recalc, so an expunge must decrement it
// by the same physical size — otherwise the count-authoritative quota read
// stays stale and a quota_warning "under" crossing never fires.
//
// Without the fallback in ExpungeMessage this fails: the aggregate keeps the
// stale 5000 because the record's vsize extension decodes to 0.
func TestExpungeVSizeFallbackNoExt(t *testing.T) {
	dir := t.TempDir()

	// Append a message with no virtual size (VSize=0, Size=0) → per-record vsize
	// extension decodes to 0.
	a := openIdx(dir, testUser)
	fa, err := a.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Deleted`}}); err != nil {
		t.Fatal(err)
	}
	idxDir := a.indexDir("INBOX")
	a.Close() //nolint:errcheck

	// The mailbox backend records the physical size in the .names sidecar.
	if err := os.WriteFile(namesPath(idxDir), []byte("1\tmsg\t5000\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reopen: fs.sizes[1]=5000 from the sidecar, record vsize ext still 0.
	b := openIdx(dir, testUser)
	fb, err := b.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Self-heal the aggregate the way the count backend does (physical fallback).
	if err := b.RecomputeVSize(fb.ID); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := b.FolderVSize(fb.ID); v != 5000 {
		t.Fatalf("precondition: aggregate=%d, want 5000 (physical-size fallback)", v)
	}

	// Expunge must drop the aggregate to 0 despite the missing per-record vsize.
	if err := b.ExpungeMessage(fb.ID, 1); err != nil {
		t.Fatal(err)
	}
	if v, m, _ := b.FolderVSize(fb.ID); v != 0 || m != 0 {
		t.Fatalf("after expunge: vsize=%d msgs=%d, want 0/0 (#570: fallback to physical size)", v, m)
	}
	b.Close() //nolint:errcheck
}
