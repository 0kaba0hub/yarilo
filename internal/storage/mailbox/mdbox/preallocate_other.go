//go:build !linux

package mdbox

import "os"

// preallocateFile is a no-op off Linux: no portable reservation syscall, and
// production runs on Linux.
func preallocateFile(_ *os.File, _ int64) error { return nil }
