package file

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Recreate refuses a base whose Header.HeaderSize disagrees with the extension
// headers it is handed, and the folder is then unflushable: no rotation, no
// optimize, and a keyword STORE that answers NO [SERVERBUG] (#1285). The size
// is now recomputed from the extensions being written, so no writer can leave
// it behind.
//
// The keyword registry is the extension that grows at runtime, and the sandbox
// hit it at 101 bytes — a length that is not a multiple of its own alignment.
// Both parities are rows here: a header length that is a multiple of 4 worked
// before this change and must keep working, and one that is not is the input
// that distinguishes.
func TestKeywordRegistryOfEitherParityFlushes(t *testing.T) {
	tests := []struct {
		name  string
		names []string
	}{
		// Encoded lengths measured: these two land on either side of the
		// alignment, which is what makes the pair distinguishing rather than
		// two spellings of one case.
		{"header length not a multiple of four", []string{"mykeyword1", "$label", "abc", "d", "ee", "fff"}},
		{"header length a multiple of four", []string{"mykeyword1", "$label", "abc", "d"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
			idx := New().OpenUser(info)
			defer idx.Close() //nolint:errcheck

			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			m := &mailbox.MessageMeta{Filename: "f", Size: 100}
			if err := idx.AllocateAndAppend(f.ID, m); err != nil {
				t.Fatalf("append: %v", err)
			}
			for _, kw := range tc.names {
				if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
					m.UID: {Mode: mailbox.FlagsAdd, Keywords: []string{kw}},
				}); err != nil {
					t.Fatalf("store %q: %v", kw, err)
				}
			}

			// A second handle reads the base from disk: the names have to be
			// there, not only in the writer's memory.
			reader := New().OpenUser(info)
			defer reader.Close() //nolint:errcheck
			rf, err := reader.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatalf("reader open: %v", err)
			}
			msgs, err := reader.GetMessages(rf.ID, mailbox.SeqSet{{From: m.UID, To: m.UID}})
			if err != nil {
				t.Fatalf("get messages: %v", err)
			}
			if len(msgs) != 1 || len(msgs[0].Keywords) != len(tc.names) {
				t.Fatalf("keywords read back = %v, want %v", msgs[0].Keywords, tc.names)
			}
		})
	}
}

// The fix has to heal an index that is already in the refused state, not only
// avoid producing new ones: the sandbox account carries one on disk, and a
// version that only stopped creating them would leave it broken forever.
func TestFlushRepairsAStaleHeaderSize(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	fs := idx.(*userHandle).ui.open[f.ID]
	// The damaged state, reproduced exactly: the extension header holds more
	// bytes than the size in the file header accounts for.
	ext := findExt(fs.file.Extensions, extNameKeywords)
	ext.HdrData = make([]byte, 101)
	ext.HdrSize = uint32(len(ext.HdrData))
	fs.file.Header.HeaderSize -= 8

	extBytes, err := mailindex.EncodeExtHeaders(fs.file.Extensions)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if fs.file.Header.HeaderSize == want {
		t.Fatalf("the state under test is not damaged: header %d already agrees", want)
	}

	if err := fs.flush(false); err != nil {
		t.Fatalf("flush over a damaged header: %v", err)
	}
	if fs.file.Header.HeaderSize != want {
		t.Errorf("header size after flush = %d, want %d", fs.file.Header.HeaderSize, want)
	}
	// And the repaired base is readable by someone who was not there for it.
	reader := New().OpenUser(info)
	defer reader.Close() //nolint:errcheck
	if _, err := reader.OpenFolder("INBOX", 0); err != nil {
		t.Errorf("reopening the repaired index: %v", err)
	}
}

// A compaction that cannot write the base is deliberately non-fatal — the
// folder keeps serving mail — but it must not be silent: rotation stopping
// looks exactly like rotation having nothing to do (#1285).
func TestRefusedCompactionIsCounted(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New(WithLogCompaction(1, 2, 0)).OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fs := idx.(*userHandle).ui.open[f.ID]

	before := counterTotal(t, "fileindex_log_compaction_refused_total")
	// A record layout the base cannot be written with: Recreate refuses, and
	// this is the state a damaged index reaches on its own.
	fs.file.Header.RecordSize++
	fs.logSize = 1 << 20
	idx.(*userHandle).ui.compactLogIfNeeded(fs)

	if got := counterTotal(t, "fileindex_log_compaction_refused_total") - before; got != 1 {
		t.Errorf("refused compactions counted = %v, want 1", got)
	}
}

func counterTotal(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var out float64
		for _, m := range mf.GetMetric() {
			out += m.GetCounter().GetValue()
		}
		return out
	}
	return 0
}
