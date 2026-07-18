//go:build !linux

package mdbox

import "os"

// preallocateFile is a no-op on non-Linux platforms — there is no portable
// block-reservation syscall, and production runs on Linux/k8s. Local dev on
// darwin/etc. simply grows the m.<N> file write-by-write. See the Linux build for
// the real fallocate.
func preallocateFile(_ *os.File, _ int64) error { return nil }
