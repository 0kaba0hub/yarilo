package msgcache

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// nil is a designed value here: Open returns it on every miss, and
// the contract is "no cache, parse as today". Every method must honour it --
// the readers panicked because a field id is evaluated before the map lookup,
// so the guard on the helper they call did not cover them (#1184).
func TestFolderCacheNilReceiverIsTheAbsentCache(t *testing.T) {
	var fc *Handle
	m := &mailbox.MessageMeta{UID: 1, CacheOffset: 4096, Filename: "m"}

	t.Run("envelope", func(t *testing.T) {
		if got := fc.Envelope(m); got != nil {
			t.Errorf("envelope = %+v, want nil", got)
		}
	})
	t.Run("bodyStructure", func(t *testing.T) {
		if got := fc.BodyStructure(m); got != nil {
			t.Errorf("bodyStructure = %+v, want nil", got)
		}
	})
	t.Run("read", func(t *testing.T) {
		if got := fc.read(m); got != nil {
			t.Errorf("read = %+v, want nil", got)
		}
	})
	t.Run("head", func(t *testing.T) {
		if got := fc.head(m); got != 0 {
			t.Errorf("head = %d, want 0", got)
		}
	})
	t.Run("store", func(t *testing.T) {
		fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "s"})
	})
	t.Run("storeBodyStructure", func(t *testing.T) {
		fc.StoreBodyStructure(m, &imaplib.BodyStructureSinglePart{Type: "text", Subtype: "plain"})
	})
	t.Run("storeField", func(t *testing.T) {
		fc.storeField(m, 0, []byte("x"))
	})
	t.Run("close", func(t *testing.T) {
		fc.Close()
	})
}

// olderIndex looks like an index written before the cache extension existed:
// the pair identity reports absent, and the real add is delegated so the rest
// of the path stays honest.
type olderIndex struct {
	mailbox.UserIndex
	ensured int
}

func (o *olderIndex) CachePairIdentity(uint64) (uint32, uint32, bool, error) {
	return 0, 0, false, nil
}

func (o *olderIndex) EnsureCacheExtension(folderID uint64) (uint32, uint32, error) {
	o.ensured++
	return o.UserIndex.(interface {
		EnsureCacheExtension(uint64) (uint32, uint32, error)
	}).EnsureCacheExtension(folderID)
}

func (o *olderIndex) BumpCacheGeneration(folderID uint64) (uint32, error) {
	return o.UserIndex.(interface {
		BumpCacheGeneration(uint64) (uint32, error)
	}).BumpCacheGeneration(folderID)
}

func (o *olderIndex) CachePath(folderID uint64) (string, error) {
	return o.UserIndex.(interface {
		CachePath(uint64) (string, error)
	}).CachePath(folderID)
}

func (o *olderIndex) SetCacheOffsets(folderID uint64, offsets map[uint32]uint32) error {
	return o.UserIndex.(interface {
		SetCacheOffsets(uint64, map[uint32]uint32) error
	}).SetCacheOffsets(folderID, offsets)
}

// An index without the extension is the normal case on an upgraded
// deployment, not an error: the session must add it and cache, not fall back
// to uncached forever (#1184).
func TestOpenFolderCacheAddsTheExtensionToAnOlderIndex(t *testing.T) {
	real := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
	f, err := real.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := real.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m"}); err != nil {
		t.Fatal(err)
	}
	idx := &olderIndex{UserIndex: real}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("an index without the extension served no cache: every mailbox in an " +
			"upgraded deployment would stay uncached forever")
	}
	defer fc.Close()
	if idx.ensured != 1 {
		t.Errorf("EnsureCacheExtension called %d times, want 1", idx.ensured)
	}
}
