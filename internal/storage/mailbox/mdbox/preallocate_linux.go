//go:build linux

package mdbox

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocateFile reserves blocks for a growing m.<N> up front. KEEP_SIZE is
// mandatory: the write offset comes from the file's logical size, so the reserve
// must not move it. Best effort.
func preallocateFile(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size)
}
