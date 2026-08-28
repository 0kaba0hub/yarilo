// Package dboxref holds dbox v2 files produced by the reference
// implementation, and hands them to the storage drivers' tests.
//
// The files are bytes another implementation wrote, not bytes ours wrote: a
// fixture built by our own writer would prove only that we agree with
// ourselves, which is what a 32-byte message header did for months while the
// reference refused to append to our files (#1522, #1526).
//
// See testdata/README.md for how they were produced and what each one covers.
package dboxref

import (
	"embed"
	"testing"
)

//go:embed testdata
var files embed.FS

// MdboxFile is a storage file carrying three records: the first preceded by the
// file-header line, the second and third not. Reading the second is the path
// that has to take the header size from the file header rather than from a
// constant, because there is no header line in front of it.
func MdboxFile(t *testing.T) []byte { return read(t, "testdata/mdbox-m.1") }

// SdboxShort and SdboxLong are single-message files. The long one's body runs
// past the window a reader peeks to find the record header, so a reader that
// happens to fit everything in one read is not what is being measured.
func SdboxShort(t *testing.T) []byte { return read(t, "testdata/sdbox-u.1") }
func SdboxLong(t *testing.T) []byte  { return read(t, "testdata/sdbox-u.2") }

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := files.ReadFile(name)
	if err != nil {
		t.Fatalf("reference fixture %s: %v", name, err)
	}
	return b
}
