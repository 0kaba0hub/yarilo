package quota

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type fakeVSizer struct {
	byName map[string]uint64    // folder name → id
	agg    map[uint64][2]uint64 // id → {bytes, messages}
}

func (f *fakeVSizer) OpenFolder(name string, _ uint32) (*mailbox.Folder, error) {
	id, ok := f.byName[name]
	if !ok {
		return nil, errors.New("no such folder")
	}
	return &mailbox.Folder{ID: id}, nil
}

func (f *fakeVSizer) FolderVSize(id uint64) (uint64, uint32, error) {
	v, ok := f.agg[id]
	if !ok {
		return 0, 0, errors.New("no aggregate")
	}
	return v[0], uint32(v[1]), nil
}

func TestCountUsage(t *testing.T) {
	f := &fakeVSizer{
		byName: map[string]uint64{"INBOX": 1, "Sent": 2, "Archive": 3},
		agg: map[uint64][2]uint64{
			1: {1000, 10},
			2: {500, 5},
			3: {250, 3},
		},
	}
	// Missing folder is skipped, not fatal.
	u := CountUsage(f, []string{"INBOX", "Sent", "Archive", "Ghost"}, Limits{})
	if u.StorageBytes != 1750 {
		t.Errorf("StorageBytes = %d, want 1750", u.StorageBytes)
	}
	if u.Messages != 18 {
		t.Errorf("Messages = %d, want 18", u.Messages)
	}

	// Empty folder list yields zero usage.
	if z := CountUsage(f, nil, Limits{}); z.StorageBytes != 0 || z.Messages != 0 {
		t.Errorf("empty = %+v, want zero", z)
	}
}
