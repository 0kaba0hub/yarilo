package all

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
)

func TestAllDriversRegistered(t *testing.T) {
	want := []string{"fail", "file", "memory", "redis", "sql"}
	got := make(map[string]bool)
	for _, n := range dict.Drivers() {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("driver %q missing after blank-import of drivers/all (got %v)", w, dict.Drivers())
		}
	}
}
