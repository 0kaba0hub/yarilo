//go:build linux

package mdbox

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocateFile reserves size bytes of disk blocks for f up front, so a growing
// m.<N> lands in contiguous, already-allocated space instead of allocating
// block-by-block on every append (mdbox_preallocate_space). FALLOC_FL_KEEP_SIZE
// is mandatory: yarilo derives each record's write offset from the file's logical
// size (O_APPEND + Stat().Size()), so the fallocate must reserve blocks WITHOUT
// changing that size — the file still grows from 0 as records are written, just
// into pre-reserved extents. Best-effort: the caller treats any error as a
// non-fatal hint.
func preallocateFile(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size)
}
