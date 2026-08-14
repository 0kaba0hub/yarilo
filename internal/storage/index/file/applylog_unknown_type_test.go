package file

import (
	"os"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record whose type the replay does not implement used to fall through the
// dispatch switch: the payload was consumed, the offset advanced, and the open
// reported a fully replayed tail over a state missing whatever that record
// said (#1314). It is refused now, which is what makes #1281's writer safe to
// roll out -- the worst a version skew can do is fail an open loudly.
func TestApplyLogRefusesAnUnknownTransactionType(t *testing.T) {
	// 0x00400000 is not assigned by the format, so no writer in any version
	// emits it: a stand-in for "a type from a newer binary".
	const unknownKind = mailindex.TxType(0x00400000)

	tests := []struct {
		name     string
		kind     mailindex.TxType
		payload  []byte
		wantRead bool // true: the tail is accepted, false: refused
	}{
		{
			name:    "a type this binary does not implement",
			kind:    unknownKind,
			payload: []byte{1, 2, 3, 4},
		},
		{
			// KEYWORD_UPDATE was the case this refusal was built for: until
			// #1281 it was a type from a newer writer and belonged here. It is
			// implemented now, so it belongs to the replay rows instead --
			// this one only records that the refusal is not what applies it.
			name: "a keyword record, now implemented",
			kind: mailindex.TxTypeKeywordUpdate,
			payload: mailindex.EncodeTxKeywordUpdatePayload(mailindex.TxKeywordUpdate{
				ModifyType: mailindex.TxKeywordModifyAdd,
				Name:       "$label1",
				UIDRanges:  []mailindex.TxKeywordUIDRange{{UID1: 1, UID2: 1}},
			}),
			wantRead: true,
		},
		{
			// A known type judged corrupt by the format's own rule is not an
			// unknown type, and must keep being ignored rather than turning
			// every unmarked expunge into a failed open.
			name:     "an expunge without its corruption-defence bit",
			kind:     mailindex.TxTypeExpungeGUID,
			payload:  make([]byte, 20),
			wantRead: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			b := openIdx(dir, testUser)
			f, err := b.OpenFolder("INBOX", 1, "")
			if err != nil {
				t.Fatalf("OpenFolder: %v", err)
			}
			uid, err := b.AllocateUID(f.ID)
			if err != nil {
				t.Fatalf("AllocateUID: %v", err)
			}
			if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "1.eml", Size: 10}); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}

			fs := b.open[f.ID]
			appendRawLogRecord(t, fs.indexPath+".log", tc.kind, tc.payload)

			fs.mu.Lock()
			fs.file, err = mailindex.Open(fs.indexPath)
			if err != nil {
				fs.mu.Unlock()
				t.Fatalf("reopen base: %v", err)
			}
			_, applyErr := fs.applyLog(0)
			fs.mu.Unlock()

			if tc.wantRead {
				if applyErr != nil {
					t.Fatalf("a record the format tells us to ignore failed the replay: %v", applyErr)
				}
				return
			}
			if applyErr == nil {
				t.Fatal("the tail was reported as replayed; a record it could not read was skipped in silence")
			}
			// The error has to say which type, or an operator cannot tell a
			// version skew from a corrupt file.
			if !strings.Contains(applyErr.Error(), "unknown transaction type") {
				t.Errorf("refusal does not name the cause: %v", applyErr)
			}
		})
	}
}

// appendRawLogRecord writes one well-formed record with the given type to the
// end of a log -- well-formed being the point: this is not a torn tail, it is
// a record a newer writer meant.
func appendRawLogRecord(t *testing.T, logPath string, kind mailindex.TxType, payload []byte) {
	t.Helper()
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer lf.Close() //nolint:errcheck // test helper
	hdr := make([]byte, 8)
	if err := mailindex.EncodeTxHeader(hdr, mailindex.TxHeader{
		Size: uint32(8 + len(payload)),
		Type: mailindex.TxTypeFlags(kind),
	}); err != nil {
		t.Fatalf("encode header: %v", err)
	}
	if _, err := lf.Write(append(hdr, payload...)); err != nil {
		t.Fatalf("write record: %v", err)
	}
}
