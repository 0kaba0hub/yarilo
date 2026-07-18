package mailboxbuild

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestParseIntervalSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0}, {"0", 0}, {"30", 30}, {"45", 45},
		{"30s", 30}, {"5m", 300}, {"1h", 3600}, {"90m", 5400},
		{"-5", 0}, {"garbage", 0}, {"10x", 0},
	}
	for _, c := range cases {
		if got := ParseIntervalSeconds(c.in); got != c.want {
			t.Errorf("ParseIntervalSeconds(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestByDriverThreadsMdboxAltStorage guards #639: ByDriver must thread
// mdbox_alt_storage_path into the mdbox backend so altmove works. A backend built
// with the key set reports alt storage enabled; one built without it does not.
func TestByDriverThreadsMdboxAltStorage(t *testing.T) {
	altEnabled := func(sc config.StorageConfig) bool {
		u := ByDriver("mdbox", sc, nil).OpenUser(&mailbox.UserInfo{Username: "u@d.test", Home: t.TempDir()})
		return u.(interface{ AltEnabled() bool }).AltEnabled()
	}
	if !altEnabled(config.StorageConfig{MdboxAltStoragePath: "/mnt/cold/%d/%n"}) {
		t.Error("mdbox_alt_storage_path set but AltEnabled() is false — alt storage not threaded (#639)")
	}
	if altEnabled(config.StorageConfig{}) {
		t.Error("no alt path configured but AltEnabled() is true")
	}
}
