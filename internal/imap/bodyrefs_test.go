package imap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A file must survive until the last record naming it is expunged, otherwise
// expunging one member of a damaged pair strips the other's body.
func TestBodyRefsFreesOnLastRelease(t *testing.T) {
	tests := []struct {
		name  string
		msgs  []*mailbox.MessageMeta
		file  string
		frees []bool // result of release() per successive call
	}{
		{
			name:  "sole record frees immediately",
			msgs:  []*mailbox.MessageMeta{{UID: 1, Filename: "a"}},
			file:  "a",
			frees: []bool{true},
		},
		{
			name:  "shared file frees only on the second release",
			msgs:  []*mailbox.MessageMeta{{UID: 1, Filename: "a"}, {UID: 2, Filename: "a"}},
			file:  "a",
			frees: []bool{false, true},
		},
		{
			name: "three records naming one file",
			msgs: []*mailbox.MessageMeta{
				{UID: 1, Filename: "a"}, {UID: 2, Filename: "a"}, {UID: 3, Filename: "a"},
			},
			file:  "a",
			frees: []bool{false, false, true},
		},
		{
			name:  "distinct files are independent",
			msgs:  []*mailbox.MessageMeta{{UID: 1, Filename: "a"}, {UID: 2, Filename: "b"}},
			file:  "a",
			frees: []bool{true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := newBodyRefs(tt.msgs)
			for i, want := range tt.frees {
				if got := refs.fate(tt.file) == bodyFree; got != want {
					t.Errorf("fate #%d frees = %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

// An empty filename means the record names no body, so there is nothing to free
// and nothing referring to one -- its own case since #1693.
func TestBodyRefsIgnoresEmptyFilename(t *testing.T) {
	refs := newBodyRefs([]*mailbox.MessageMeta{{UID: 1, Filename: ""}})
	if got := refs.fate(""); got != bodyNameless {
		t.Errorf("fate(\"\") = %v, want bodyNameless", got)
	}
}

// A file no record names is freeable: nothing else can be pointing at it, and
// the caller only releases a file it is expunging anyway.
func TestBodyRefsUnknownFileIsFreeable(t *testing.T) {
	refs := newBodyRefs([]*mailbox.MessageMeta{{UID: 1, Filename: "a"}})
	if refs.fate("b") != bodyFree {
		t.Error("unknown file reported as still referenced")
	}
}
