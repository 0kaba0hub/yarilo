package threads

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func buildSidecar(tb testing.TB, msgs int) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), FileName)
	st, _ := Load(path)
	for i := 0; i < msgs; i++ {
		p := Placement{
			GUID:       fmt.Sprintf("%032x", i),
			MessageID:  fmt.Sprintf("msg-%d@example.com", i),
			SubjectKey: fmt.Sprintf("subject number %d", i%(msgs/10+1)),
			ThreadID:   fmt.Sprintf("%032x", i%(msgs/5+1)),
		}
		if err := Append(path, st, p); err != nil {
			tb.Fatal(err)
		}
	}
	fi, _ := os.Stat(path)
	tb.Logf("%d messages -> %d KiB", msgs, fi.Size()/1024)
	return path
}

func BenchmarkLoad(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		path := buildSidecar(b, n)
		b.Run(fmt.Sprintf("%d-messages", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Load(path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
