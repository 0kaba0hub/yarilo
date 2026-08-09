package mimesalvage

import (
	"bufio"
	"bytes"
	"io"
)

// replayReader remembers what was read from it, so a failed parse can be
// retried from the beginning without the caller providing a seekable stream.
//
// It keeps the whole message in memory only in the failing case: the copy is
// filled as the parser reads, and dropped as soon as the parse succeeds.
type replayReader struct {
	src  *bufio.Reader
	seen bytes.Buffer
}

func (r *replayReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.seen.Write(p[:n])
	}
	return n, err
}

// replay returns the message from its first byte: what the parser consumed,
// followed by what it never reached.
func (r *replayReader) replay() io.Reader {
	return io.MultiReader(bytes.NewReader(r.seen.Bytes()), r.src)
}
