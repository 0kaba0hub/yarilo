package file

import (
	"errors"
	"os"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// logReader is one open descriptor on the log, and everything a refresh needs
// read through it: identity, size, header, body.
//
// It exists because those four used to come from three separate opens. Under
// the cross-process lock that was invisible — no sibling could compact in
// between. Without the lock it is a torn view with extra steps: the header says
// "this log belongs to your base, replay it whole", the body belongs to the log
// that replaced it, and the replay starts at an offset that means nothing in
// the file it is reading.
//
// A nil file is a log that is not there, which is a normal state: a folder
// whose base was just written has no log until the next append.
type logReader struct {
	f    *os.File
	stat os.FileInfo
	hdr  mailindex.LogHeader
	ok   bool // a header was read; false for absent, empty or unreadable
	size int64
}

func openLogRead(indexPath string) (*logReader, error) {
	lg := &logReader{}
	f, err := os.Open(indexPath + ".log")
	if errors.Is(err, os.ErrNotExist) {
		return lg, nil
	}
	if err != nil {
		return lg, err
	}
	lg.f = f
	st, serr := f.Stat()
	if serr != nil {
		_ = f.Close()
		lg.f = nil
		return lg, serr
	}
	lg.stat = st
	lg.size = st.Size()
	if hdr, herr := mailindex.DecodeLogHeader(f); herr == nil {
		lg.hdr = hdr
		lg.ok = true
	}
	return lg, nil
}

// lineage is what the log announces about the base it belongs to. Zero when
// there is no readable header, which reads as "proves nothing".
func (lg *logReader) lineage() uint32 {
	if !lg.ok {
		return lineageUnknown
	}
	return lg.hdr.FileSeq
}

func (lg *logReader) close() {
	if lg.f != nil {
		_ = lg.f.Close()
		lg.f = nil
	}
}
