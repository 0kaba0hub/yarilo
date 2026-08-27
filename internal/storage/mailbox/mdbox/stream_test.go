package mdbox

import (
	"bytes"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fetch hands back a reader over the body, not the body itself.
//
// It used to read the whole record into memory before returning. A client
// asking for HEADER.FIELDS of a 500 KB message then cost 500 KB of allocation
// and I/O to produce 2 KB, and under a one-CPU quota the garbage from that,
// multiplied across parallel sessions, is what parked FETCH commands for
// seconds -- the goroutine captured at a stall stood on the make([]byte, size)
// with GC at ~28% of the CPU profile (#1517). maildir returns the file and
// never paid it.
//
// Two things are asserted. The bytes are identical to what was saved, read
// through the reader in full, so streaming changed nothing about the data.
// And reading only the head of a large message allocates a small, fixed
// amount rather than the message's size: that is the property the stall was
// about, and a slurping Fetch fails it by a factor of a thousand.
func TestFetchStreamsTheBodyInsteadOfSlurpingIt(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	head := "From: a@a.com\r\nSubject: big\r\n\r\n"
	msg := head + strings.Repeat("x", 512<<10) + "\r\n"
	fn := mustSave(t, u, msg)

	// Whole body, byte for byte.
	rc, err := u.Fetch("INBOX", fn, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(got, []byte(msg)) {
		t.Fatalf("body differs: got %d bytes, want %d", len(got), len(msg))
	}

	// Only the head, as a HEADER.FIELDS fetch reads it. The bound is the
	// per-call overhead of positioning on the record (the 64-byte window, the
	// 32-byte header, the section reader, the closer) plus the 4 KB we read
	// here -- nowhere near the 512 KB a slurp allocates.
	buf := make([]byte, 4096)
	var before, after runtimeMemStats
	before.read()
	for i := 0; i < 20; i++ {
		rc, err := u.Fetch("INBOX", fn, false)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		_, _ = io.ReadFull(rc, buf)
		_ = rc.Close()
	}
	after.read()
	perFetch := (after.totalAlloc - before.totalAlloc) / 20
	// A slurp allocates the whole 512 KB every time; streaming the head
	// allocates the fixed positioning overhead. 64 KB is a generous ceiling
	// for the latter and an eighth of the former.
	if perFetch > 64<<10 {
		t.Fatalf("reading the head of a 512 KB message allocated %d bytes per Fetch; "+
			"the body is being read in full to serve a few KB of it", perFetch)
	}
}

// runtimeMemStats is the one field of runtime.MemStats this test needs, behind
// a name so the intent reads at the call site.
type runtimeMemStats struct{ totalAlloc uint64 }

func (m *runtimeMemStats) read() {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	m.totalAlloc = ms.TotalAlloc
}
